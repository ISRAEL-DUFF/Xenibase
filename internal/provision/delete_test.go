package provision

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

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

// A delete dates the project's nightly and archive backups on platform
// storage with its final backup's expiry, so the hourly expiry removes them
// (V2 §10.10); backups in the organisation's own bucket are left alone.
func TestDeleteExpiresEarlierBackups(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, create := h.create(t)
	if _, err := h.db.Exec(ctx, `UPDATE projects SET status = 'active' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(ctx, `UPDATE operations SET status = 'succeeded' WHERE id = $1`, create.ID); err != nil {
		t.Fatal(err)
	}
	var platform, own uuid.UUID
	if err := h.db.QueryRow(ctx, `INSERT INTO storage_targets (name, endpoint, bucket, credentials) VALUES ('platform', 'https://s3', 'b', '\x00') RETURNING id`).Scan(&platform); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(ctx, `INSERT INTO storage_targets (name, endpoint, bucket, credentials, org_id) VALUES ('own', 'https://s3', 'o', '\x00', $1) RETURNING id`, h.org).Scan(&own); err != nil {
		t.Fatal(err)
	}
	backup := func(kind string, target *uuid.UUID, status string) uuid.UUID {
		var id uuid.UUID
		if err := h.db.QueryRow(ctx, `INSERT INTO backups (project_id, kind, object_key, started_at, finished_at, status, storage_target_id)
			VALUES ($1, $2, $2 || gen_random_uuid(), now(), now(), $3, $4) RETURNING id`, p.ID, kind, status, target).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	nightly, copied, archive, v1 := backup("logical", &platform, "succeeded"), backup("logical", &platform, "copied"), backup("archive", &platform, "succeeded"), backup("logical", nil, "succeeded")
	inOwnBucket, failed := backup("logical", &own, "succeeded"), backup("logical", &platform, "failed")
	finalExpiry := time.Now().Add(29 * 24 * time.Hour).Truncate(time.Second)
	h.s.FinalBackup = func(ctx context.Context, p store.Project, _ *jobs.StepLogger) error {
		_, err := h.db.Exec(ctx, `INSERT INTO backups (project_id, kind, object_key, started_at, finished_at, status, storage_target_id, expires_at)
			VALUES ($1, 'final', 'final', now(), now(), 'succeeded', $2, $3)`, p.ID, platform, finalExpiry)
		return err
	}
	op, err := h.s.Delete(ctx, p.ID, p.Name, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	op, log := h.claim(t, op)
	if err := h.s.runDelete(ctx, op, log); err != nil {
		t.Fatal(err)
	}
	expiry := func(id uuid.UUID) *time.Time {
		var at *time.Time
		if err := h.db.QueryRow(ctx, `SELECT expires_at FROM backups WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	for name, id := range map[string]uuid.UUID{"nightly": nightly, "copied": copied, "archive": archive, "v1 (default target)": v1} {
		if at := expiry(id); at == nil || !at.Equal(finalExpiry) {
			t.Errorf("%s backup expires at %v, want the final backup's %v", name, at, finalExpiry)
		}
	}
	for name, id := range map[string]uuid.UUID{"own bucket": inOwnBucket, "failed": failed} {
		if at := expiry(id); at != nil {
			t.Errorf("%s backup got an expiry: %v", name, at)
		}
	}

	// Without a final backup they go 30 days after the delete.
	q := store.New(h.db)
	if _, err := h.db.Exec(ctx, `UPDATE backups SET expires_at = NULL WHERE id = $1`, nightly); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(ctx, `DELETE FROM backups WHERE kind = 'final'`); err != nil {
		t.Fatal(err)
	}
	if _, err := q.ExpireDeletedProjectBackups(ctx, store.ExpireDeletedProjectBackupsParams{ProjectID: p.ID, ExpiresAt: time.Now().Add(DeletedBackupsKept)}); err != nil {
		t.Fatal(err)
	}
	if at := expiry(nightly); at == nil || time.Until(*at) < 29*24*time.Hour || time.Until(*at) > 31*24*time.Hour {
		t.Fatalf("without a final backup the nightly expires at %v, want 30 days on", at)
	}
}
