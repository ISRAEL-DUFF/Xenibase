package store_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestRestrictLocksMigration: migration 00090 queues apply_settings,
// which revokes pg_locks and the lock functions from PUBLIC, for every
// active shared project, and skips dedicated, archived and deleted ones
// and one with an exclusive operation already pending.
func TestRestrictLocksMigration(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewEmpty(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.MigrateTo(ctx, pool, 89, log); err != nil {
		t.Fatal(err)
	}
	org, node, inst := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'O', 'o', id FROM quota_plans LIMIT 1`, org)
	exec(`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', 'fp', '{}')`, node)
	exec(`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	project := func(name, tier, lifecycle string, deleted bool) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO projects (id, org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region, lifecycle, deleted_at)
			VALUES ($1, $2, $3, $3, 'p_' || $3, 'p_' || $3 || '_owner', 'x', $4, $5, 'active', 'eu-central', $6, CASE WHEN $7 THEN now() END)`,
			id, org, name, tier, inst, lifecycle, deleted)
		return id
	}
	shared := project("shared", "shared", "active", false)
	busy := project("busy", "shared", "active", false)
	dedicated := project("dedicated", "dedicated", "active", false)
	archived := project("archived", "shared", "archived", false)
	gone := project("gone", "shared", "active", true)
	exec(`INSERT INTO operations (kind, project_id) VALUES ('rotate', $1)`, busy)

	if err := store.MigrateTo(ctx, pool, 90, log); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		id   uuid.UUID
		want int
	}{{"shared", shared, 1}, {"busy", busy, 0}, {"dedicated", dedicated, 0}, {"archived", archived, 0}, {"deleted", gone, 0}} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE project_id = $1 AND kind = 'apply_settings' AND status = 'queued'`, c.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != c.want {
			t.Errorf("%s project: %d apply_settings queued, want %d", c.name, n, c.want)
		}
	}
}
