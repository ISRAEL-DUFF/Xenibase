package backup

import (
	"context"
	"errors"
	"testing"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
)

// An archive backup is marked verified only when its restore test passed:
// the archive drops the project's database only after that (V3 §4.3).
func TestVerifyArchiveRecordsOnlyAPassedRestore(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.project(t, "archived")
	st := h.bucket(t, "archives")
	target := h.target(t, nil, "archives", st)

	// It passes: the row records it, and a second call doesn't restore again.
	restores := 0
	h.s.verify = func(context.Context, store.Project, store.Backup, *jobs.StepLogger) error { restores++; return nil }
	b := h.backup(t, p, Archive, target, st, "dump")
	if b.VerifiedAt != nil {
		t.Fatal("a backup just taken is already marked verified")
	}
	v, err := h.s.VerifyArchive(ctx, p, b, nil)
	if err != nil || v.VerifiedAt == nil {
		t.Fatalf("verify: %v (verified at %v), want it marked verified", err, v.VerifiedAt)
	}
	if row, _ := store.New(h.db).GetBackup(ctx, b.ID); row.VerifiedAt == nil {
		t.Error("the backup row has no verification recorded")
	}
	if _, err := h.s.VerifyArchive(ctx, p, v, nil); err != nil || restores != 1 {
		t.Errorf("verifying a verified backup again: %v, %d restore(s), want none more", err, restores)
	}

	// It fails: the backup is deleted, unverified.
	h.s.verify = func(context.Context, store.Project, store.Backup, *jobs.StepLogger) error {
		return jobs.Permanent(errors.New("pg_restore: invalid dump"))
	}
	bad := h.backup(t, p, Archive, target, st, "corrupt")
	if _, err := h.s.VerifyArchive(ctx, p, bad, nil); err == nil {
		t.Fatal("a failed restore test verified the backup")
	}
	row, err := store.New(h.db).GetBackup(ctx, bad.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "deleted" || row.VerifiedAt != nil || exists(t, st, bad.ObjectKey) {
		t.Errorf("after a failed restore test the backup is %s (verified at %v, object stored %t), want deleted and unverified",
			row.Status, row.VerifiedAt, exists(t, st, bad.ObjectKey))
	}
}

// When the restore test fails and the backup can't be deleted either, the
// row stays succeeded but unverified, which is what an archive retry finds
// (as after a crash during the verification); it verifies it again.
func TestVerifyArchiveKeepsAnUndeletableBackupUnverified(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.project(t, "stuck")
	f, err := storage.NewFake("flaky")
	if err != nil {
		t.Fatal(err)
	}
	st := f.Target("flaky")
	target := h.target(t, nil, "flaky", st)
	b := h.backup(t, p, Archive, target, st, "dump")
	f.Close() // the storage is unreachable: the delete fails

	h.s.verify = func(context.Context, store.Project, store.Backup, *jobs.StepLogger) error {
		return errors.New("the server stopped")
	}
	if _, err := h.s.VerifyArchive(ctx, p, b, nil); err == nil {
		t.Fatal("a failed restore test verified the backup")
	}
	row, err := store.New(h.db).GetBackup(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "succeeded" || row.VerifiedAt != nil {
		t.Fatalf("the undeletable backup is %s, verified at %v; want succeeded and unverified", row.Status, row.VerifiedAt)
	}

	h.s.verify = func(context.Context, store.Project, store.Backup, *jobs.StepLogger) error { return nil }
	v, err := h.s.VerifyArchive(ctx, p, row, nil)
	if err != nil || v.VerifiedAt == nil {
		t.Fatalf("verifying it again: %v (verified at %v)", err, v.VerifiedAt)
	}
}
