package backup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
)

// A target at another live target's endpoint, bucket and prefix is
// refused, however the prefix is written and whoever owns the other one.
func TestSaveTargetRefusesATakenLocation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.bucket(t, "b")
	st.Prefix = "backups"
	first := h.target(t, nil, "first", st)

	again := st
	again.Prefix = "backups/"
	_, _, err := h.s.SaveTarget(ctx, nil, nil, input("second", again), nil)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), `"first"`) {
		t.Fatalf("a second platform target at the same place: %v, want a conflict naming the first", err)
	}
	_, _, err = h.s.SaveTarget(ctx, &h.org, nil, input("ours", st), nil)
	if !errors.Is(err, ErrConflict) || strings.Contains(err.Error(), "first") {
		t.Fatalf("an org target at a platform target's place: %v, want a conflict that does not name it", err)
	}

	// Saving the target itself again, or another prefix, is fine.
	if _, _, err := h.s.SaveTarget(ctx, nil, &first.ID, input("first", again), nil); err != nil {
		t.Fatalf("re-saving the target: %v", err)
	}
	other := st
	other.Prefix = "backups/other"
	h.target(t, &h.org, "ours", other)
}

// Rows saved before the check can still name one place twice: a copy
// between them, deleting an original whose only copy is there, and deleting
// a cross-region copy there are all refused, and the object stays.
func TestCopyBetweenTargetsAtOnePlaceIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	q := store.New(h.db)
	st := h.bucket(t, "b")
	a := h.target(t, nil, "a", st)
	twin := h.twin(t, a, "a-again")
	p := h.project(t, "app")
	b := h.backup(t, p, Logical, a, st, "the only copy")

	dst, err := storage.New(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.copyBackup(ctx, b, twin.ID, dst); !errors.Is(err, errSameLocation) {
		t.Fatalf("copy onto the same place: %v, want it refused", err)
	}
	if got := status(t, h, b.ID); got != "succeeded" {
		t.Fatalf("the original is %s after a refused copy", got)
	}

	// As a switch before the check would have left it: a "verified" copy
	// at the same place, the original marked copied.
	if _, err := q.InsertBackup(ctx, store.InsertBackupParams{ProjectID: &p.ID, Kind: Logical, ObjectKey: b.ObjectKey, StorageTargetID: &twin.ID, KeyWrapped: b.KeyWrapped, CopyOf: &b.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(ctx, `UPDATE backups SET status = 'succeeded' WHERE copy_of = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.MarkBackupCopied(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	b, _ = q.GetBackup(ctx, b.ID)
	if err := h.s.deleteOriginal(ctx, b); err == nil {
		t.Fatal("deleted an original whose only copy is at the same place")
	}
	if !exists(t, st, b.ObjectKey) || status(t, h, b.ID) != "copied" {
		t.Fatal("the object went with the refused delete")
	}

	// A cross-region "copy" at the primary's own place is the primary.
	c := h.backup(t, p, Logical, a, st, "nightly")
	if err := q.SetBackupCopy(ctx, store.SetBackupCopyParams{ID: c.ID, CopyStatus: ptr(CopyCopied), CopyTargetID: &twin.ID}); err != nil {
		t.Fatal(err)
	}
	c, _ = q.GetBackup(ctx, c.ID)
	if err := h.s.deleteCopy(ctx, c); err != nil {
		t.Fatal(err)
	}
	if !exists(t, st, c.ObjectKey) {
		t.Fatal("removing the cross-region copy deleted the backup itself")
	}
	if err := h.s.copyObject(ctx, c, dst); !errors.Is(err, errSameLocation) {
		t.Fatalf("a cross-region copy onto the same place: %v, want it refused", err)
	}
}

// twin adds a target row at row's place, as one saved before the check.
func (h *harness) twin(t *testing.T, row store.StorageTarget, name string) store.StorageTarget {
	t.Helper()
	ctx := context.Background()
	st, err := h.s.openTarget(row)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	b, _ := json.Marshal(creds{AccessKey: st.AccessKey, SecretKey: st.SecretKey, Region: st.Region, PathStyle: st.PathStyle})
	sealed, err := h.s.keyring.Encrypt(b, credsAAD(id))
	if err != nil {
		t.Fatal(err)
	}
	out, err := store.New(h.db).InsertStorageTarget(ctx, store.InsertStorageTargetParams{
		ID: id, OrgID: row.OrgID, Name: name, Endpoint: row.Endpoint + "/", Bucket: row.Bucket, Prefix: row.Prefix,
		Region: row.Region, PathStyle: row.PathStyle, Credentials: sealed,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
