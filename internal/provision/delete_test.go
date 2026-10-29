package provision

import (
	"context"
	"fmt"
	"testing"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
)

// A delete retried after an attempt that had already torn the project
// down skips the final backup of the database that is gone, and finishes.
func TestDeleteRetryAfterTeardownFinishes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, create := h.create(t)
	if _, err := h.db.Exec(ctx, `UPDATE projects SET status = 'active' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(ctx, `UPDATE operations SET status = 'succeeded' WHERE id = $1`, create.ID); err != nil {
		t.Fatal(err)
	}
	backups := 0
	h.s.FinalBackup = func(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
		backups++
		if !h.databaseExists(t, p.DbName) {
			return fmt.Errorf("pg_dump: database %q does not exist", p.DbName)
		}
		return log.Info(ctx, "backup", "final backup taken")
	}
	op, err := h.s.Delete(ctx, p.ID, p.Name, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err = store.New(h.db).GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The first attempt took the backup and tore down, then failed.
	op, log := h.claim(t, op)
	if err := h.s.FinalBackup(ctx, p, log); err != nil {
		t.Fatal(err)
	}
	if err := h.s.teardown(ctx, p, log); err != nil {
		t.Fatal(err)
	}

	if err := h.s.runDelete(ctx, op, log); err != nil {
		t.Fatalf("retried delete: %v", err)
	}
	if backups != 1 {
		t.Fatalf("final backup attempted %d times, want 1", backups)
	}
	got, err := store.New(h.db).GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDeleted || got.DeletedAt == nil {
		t.Fatalf("project is %s after the delete", got.Status)
	}
}
