package backup

import (
	"context"
	"testing"

	"github.com/israel-duff/xenibase/internal/store"
)

// A deleted project's nightly backups, dated by the delete, are removed by
// the hourly expiry: the object, its cross-region copy and the row; an
// original a storage switch copied goes too, though it is "copied" rather
// than "succeeded".
func TestExpiryRemovesDeletedProjectBackups(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	q := store.New(h.db)
	p := h.project(t, "gone")
	oldSt, newSt, copySt := h.bucket(t, "old"), h.bucket(t, "new"), h.bucket(t, "copy")
	oldT, newT, copyT := h.target(t, nil, "old", oldSt), h.target(t, nil, "new", newSt), h.target(t, nil, "copy", copySt)

	nightly := h.backup(t, p, Logical, newT, newSt, "nightly")
	put(t, copySt, nightly.ObjectKey, "nightly")
	if err := q.SetBackupCopy(ctx, store.SetBackupCopyParams{ID: nightly.ID, CopyStatus: ptr(CopyCopied), CopyTargetID: &copyT.ID}); err != nil {
		t.Fatal(err)
	}
	original := h.backup(t, p, Logical, oldT, oldSt, "switched")
	if err := q.MarkBackupCopied(ctx, original.ID); err != nil {
		t.Fatal(err)
	}

	if err := q.SoftDeleteProject(ctx, store.SoftDeleteProjectParams{ID: p.ID, Status: "deleted"}); err != nil {
		t.Fatal(err)
	}
	n, err := q.ExpireDeletedProjectBackups(ctx, store.ExpireDeletedProjectBackupsParams{ProjectID: p.ID, ExpiresAt: timeAgo(1)})
	if err != nil || n != 2 {
		t.Fatalf("expired %d backup(s) (%v), want 2", n, err)
	}
	h.s.expireSpecial(ctx)

	for name, b := range map[string]store.Backup{"nightly": nightly, "switched original": original} {
		if got := status(t, h, b.ID); got != "deleted" {
			t.Errorf("%s backup is %s after the expiry, want deleted", name, got)
		}
	}
	if exists(t, newSt, nightly.ObjectKey) || exists(t, copySt, nightly.ObjectKey) {
		t.Error("the nightly backup's object or its cross-region copy is still stored")
	}
	if exists(t, oldSt, original.ObjectKey) {
		t.Error("the copied original's object is still stored")
	}
}
