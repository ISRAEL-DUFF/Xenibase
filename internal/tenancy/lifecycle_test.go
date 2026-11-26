package tenancy_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store/storetest"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// Finishing an organisation's deletion dates its projects' final backups,
// and their nightly and archive backups on platform storage, 30 days on
// (V2 §10.10); backups in the organisation's own bucket are left alone.
func TestOrgDeletionExpiresBackups(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	org, node, inst, p := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id, status, delete_after)
		SELECT $1, 'O', 'o', id, 'deleting', now() - interval '1 hour' FROM quota_plans LIMIT 1`, org)
	exec(t, db, `INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', 'fp', '{}')`, node)
	exec(t, db, `INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	exec(t, db, `INSERT INTO projects (id, org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region, deleted_at)
		VALUES ($1, $2, 'p', 'p', 'p_p', 'p_p_owner', 'x', 'shared', $3, 'deleted', 'eu-central', now())`, p, org, inst)
	var platform, own uuid.UUID
	if err := db.QueryRow(ctx, `INSERT INTO storage_targets (name, endpoint, bucket, credentials) VALUES ('platform', 'https://s3', 'b', '\x00') RETURNING id`).Scan(&platform); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO storage_targets (name, endpoint, bucket, credentials, org_id) VALUES ('own', 'https://s3', 'o', '\x00', $1) RETURNING id`, org).Scan(&own); err != nil {
		t.Fatal(err)
	}
	backup := func(kind string, target uuid.UUID) uuid.UUID {
		var id uuid.UUID
		if err := db.QueryRow(ctx, `INSERT INTO backups (project_id, kind, object_key, started_at, finished_at, status, storage_target_id)
			VALUES ($1, $2, $2 || gen_random_uuid(), now(), now(), 'succeeded', $3) RETURNING id`, p, kind, target).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	final, nightly, archive, inOwnBucket := backup("final", platform), backup("logical", platform), backup("archive", platform), backup("logical", own)

	now := time.Now()
	svc := tenancy.New(db, nil, nil, tenancy.Config{Now: func() time.Time { return now }}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc.FinishOrgDeletions(ctx); err != nil {
		t.Fatal(err)
	}
	expiry := func(id uuid.UUID) *time.Time {
		var at *time.Time
		if err := db.QueryRow(ctx, `SELECT expires_at FROM backups WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	want := now.Add(30 * 24 * time.Hour)
	for name, id := range map[string]uuid.UUID{"final": final, "nightly": nightly, "archive": archive} {
		if at := expiry(id); at == nil || at.Sub(want).Abs() > time.Second {
			t.Errorf("%s backup expires at %v, want %v", name, at, want)
		}
	}
	if at := expiry(inOwnBucket); at != nil {
		t.Errorf("a backup in the organisation's own bucket got an expiry: %v", at)
	}
}
