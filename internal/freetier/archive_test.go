package freetier

import (
	"context"
	"errors"
	"testing"

	"github.com/israel-duff/xenibase/internal/store"
)

// A retry of an archive that finds the earlier attempt's backup succeeded
// but never verified (the server stopped during the verification) must not
// drop the database on it: it verifies the backup first, and drops only
// once that passed.
func TestArchiveRetryVerifiesAnUnverifiedBackupBeforeDropping(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.project(t, Paused)
	op, log := h.operation(t, p, KindArchive)
	b := h.archiveBackup(t, p, op, false)

	// The verification fails this time: the database stays.
	fake := &fakeArchiver{db: h.db, fail: errors.New("verify the archive backup: pg_restore: invalid dump")}
	h.s.archives = fake
	if err := h.s.runArchive(ctx, op, log); err == nil {
		t.Fatal("the archive succeeded on a backup that failed its verification")
	}
	if len(fake.verified) != 1 || fake.verified[0] != b.ID {
		t.Fatalf("verified %v, want the earlier attempt's backup %s verified again", fake.verified, b.ID)
	}
	if !h.hasDatabase(t, p) {
		t.Fatal("the database was dropped on an unverified backup")
	}
	if got := h.get(t, p.ID); got.Lifecycle != Paused || got.ArchiveBackupID != nil {
		t.Fatalf("the project is %s with archive backup %v, want it still paused with none", got.Lifecycle, got.ArchiveBackupID)
	}

	// The next attempt's verification passes: now it is archived and dropped.
	fake.fail = nil
	if err := h.s.runArchive(ctx, op, log); err != nil {
		t.Fatal(err)
	}
	if h.hasDatabase(t, p) {
		t.Error("the database is still there after a verified archive")
	}
	got := h.get(t, p.ID)
	if got.Lifecycle != Archived || got.ArchiveBackupID == nil || *got.ArchiveBackupID != b.ID {
		t.Errorf("the project is %s with archive backup %v, want archived on %s", got.Lifecycle, got.ArchiveBackupID, b.ID)
	}
	if row, _ := store.New(h.db).GetBackup(ctx, b.ID); row.VerifiedAt == nil {
		t.Error("the archive backup has no verification recorded")
	}
	if fake.archived != 0 {
		t.Errorf("took %d new backup(s); the earlier one was reusable", fake.archived)
	}
}

// The other way an unverified backup is left: the verification failed and
// deleting the backup failed too. With the real backup service (no agent:
// the restore can't run, and the backup's storage target doesn't exist),
// the retry tries the verification again, fails, and keeps the database.
func TestArchiveRetryKeepsTheDatabaseWhenTheBackupCannotBeVerified(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.project(t, Paused)
	op, log := h.operation(t, p, KindArchive)
	b := h.archiveBackup(t, p, op, false)

	for attempt := 1; attempt <= 2; attempt++ {
		if err := h.s.runArchive(ctx, op, log); err == nil {
			t.Fatalf("attempt %d: the archive succeeded without a verified backup", attempt)
		}
		if !h.hasDatabase(t, p) {
			t.Fatalf("attempt %d: the database was dropped on an unverified backup", attempt)
		}
	}
	row, err := store.New(h.db).GetBackup(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.VerifiedAt != nil {
		t.Errorf("the backup is marked verified (%v) though its restore never ran", row.VerifiedAt)
	}
	if got := h.get(t, p.ID); got.Lifecycle != Paused {
		t.Errorf("the project is %s, want still paused", got.Lifecycle)
	}
}

// A verified backup from an earlier attempt is used as it is.
func TestArchiveRetryUsesAVerifiedBackup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p := h.project(t, Paused)
	op, log := h.operation(t, p, KindArchive)
	h.archiveBackup(t, p, op, true)
	fake := &fakeArchiver{db: h.db, fail: errors.New("not called")}
	h.s.archives = fake
	if err := h.s.runArchive(ctx, op, log); err != nil {
		t.Fatal(err)
	}
	if len(fake.verified) != 0 || fake.archived != 0 {
		t.Errorf("verified %d and archived %d time(s), want neither", len(fake.verified), fake.archived)
	}
	if h.hasDatabase(t, p) || h.get(t, p.ID).Lifecycle != Archived {
		t.Error("the project isn't archived with its database dropped")
	}
}
