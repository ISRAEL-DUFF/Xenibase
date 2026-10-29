package store_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestDeletedProjectBackupsMigration: migration 00075 dates the nightly
// backups of projects deleted before it with their final backup, but at
// least a week on, and leaves live projects' and org targets' alone.
func TestDeletedProjectBackupsMigration(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewEmpty(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.MigrateTo(ctx, pool, 70, log); err != nil {
		t.Fatal(err)
	}
	org, node, inst, platform, own := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'O', 'o', id FROM quota_plans LIMIT 1`, org)
	exec(`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', 'fp', '{}')`, node)
	exec(`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	exec(`INSERT INTO storage_targets (id, name, endpoint, bucket, credentials) VALUES ($1, 'platform', 'https://s3', 'b', '\x00')`, platform)
	exec(`INSERT INTO storage_targets (id, name, endpoint, bucket, credentials, org_id) VALUES ($1, 'own', 'https://s3', 'o', '\x00', $2)`, own, org)
	project := func(name string, deletedAgo *time.Duration) uuid.UUID {
		id := uuid.New()
		var at *time.Time
		if deletedAgo != nil {
			v := time.Now().Add(-*deletedAgo)
			at = &v
		}
		exec(`INSERT INTO projects (id, org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region, deleted_at)
			VALUES ($1, $2, $3, $3, 'p_' || $3, 'p_' || $3 || '_owner', 'x', 'shared', $4, 'active', 'eu-central', $5)`, id, org, name, inst, at)
		return id
	}
	backup := func(p uuid.UUID, kind string, target uuid.UUID, expires *time.Time) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO backups (project_id, kind, object_key, started_at, finished_at, status, storage_target_id, expires_at)
			VALUES ($1, $2, $2 || gen_random_uuid(), now(), now(), 'succeeded', $3, $4) RETURNING id`, p, kind, target, expires).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	longAgo, recently := 60*24*time.Hour, 2*24*time.Hour
	old, recent, live := project("old", &longAgo), project("recent", &recently), project("live", nil)
	finalAt := time.Now().Add(28 * 24 * time.Hour).Truncate(time.Second)
	backup(recent, "final", platform, &finalAt)
	oldNightly, recentNightly, recentArchive := backup(old, "logical", platform, nil), backup(recent, "logical", platform, nil), backup(recent, "archive", platform, nil)
	inOwnBucket, liveNightly := backup(old, "logical", own, nil), backup(live, "logical", platform, nil)

	if err := store.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	expiry := func(id uuid.UUID) *time.Time {
		var at *time.Time
		if err := pool.QueryRow(ctx, `SELECT expires_at FROM backups WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if at := expiry(oldNightly); at == nil || time.Until(*at) < 6*24*time.Hour || time.Until(*at) > 8*24*time.Hour {
		t.Errorf("a long-deleted project's nightly expires at %v, want a week on", at)
	}
	for name, id := range map[string]uuid.UUID{"nightly": recentNightly, "archive": recentArchive} {
		if at := expiry(id); at == nil || !at.Equal(finalAt) {
			t.Errorf("a recently deleted project's %s expires at %v, want its final backup's %v", name, at, finalAt)
		}
	}
	for name, id := range map[string]uuid.UUID{"own bucket": inOwnBucket, "live project": liveNightly} {
		if at := expiry(id); at != nil {
			t.Errorf("%s backup got an expiry: %v", name, at)
		}
	}
}
