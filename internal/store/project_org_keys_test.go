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

// TestProjectOrgKeysOnLiveData upgrades a database where an earlier
// transfer left children under the old organisation: migrations 00068 and
// 00069 put them right and validate the keys, and 00068's Down removes them.
func TestProjectOrgKeysOnLiveData(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewEmpty(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.MigrateTo(ctx, pool, 60, log); err != nil {
		t.Fatal(err)
	}
	user, oldOrg, newOrg, node, inst, project := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, stmt := range []string{
		`INSERT INTO users (id, email, password_hash) VALUES ('` + user.String() + `', 'u@example.com', 'x')`,
		`INSERT INTO organizations (id, name, slug, plan_id) SELECT '` + oldOrg.String() + `', 'Old', 'old', id FROM quota_plans LIMIT 1`,
		`INSERT INTO organizations (id, name, slug, plan_id) SELECT '` + newOrg.String() + `', 'New', 'new', id FROM quota_plans LIMIT 1`,
		`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ('` + node.String() + `', 'n1', '10.0.0.1', 'both', 'fp', '{}')`,
		`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ('` + inst.String() + `', '` + node.String() + `', 'shared', 18, 5432, 'running')`,
		`INSERT INTO projects (id, org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region)
			VALUES ('` + project.String() + `', '` + newOrg.String() + `', 'app', 'app', 'p_app', 'p_app_owner', 'x', 'shared', '` + inst.String() + `', 'active', 'eu-central')`,
		// Left behind by a transfer from oldOrg.
		`INSERT INTO reaped_sessions (org_id, project_id, kind, role_name, duration_s) VALUES ('` + oldOrg.String() + `', '` + project.String() + `', 'statement', 'r', 1)`,
		`INSERT INTO editor_preferences (org_id, project_id, user_id) VALUES ('` + oldOrg.String() + `', '` + project.String() + `', '` + user.String() + `')`,
		`INSERT INTO dedicated_requests (org_id, project_id, requested_by, profile) VALUES ('` + oldOrg.String() + `', '` + project.String() + `', '` + user.String() + `', '{}')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := store.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"reaped_sessions", "editor_preferences", "dedicated_requests"} {
		var org uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT org_id FROM `+table+` WHERE project_id = $1`, project).Scan(&org); err != nil || org != newOrg {
			t.Errorf("%s: org %v, want the project's %v (%v)", table, org, newOrg, err)
		}
	}
	var keys, valid int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE convalidated) FROM pg_constraint WHERE conname LIKE '%_project_org_fkey'`).Scan(&keys, &valid); err != nil {
		t.Fatal(err)
	}
	if keys != 6 || valid != 6 {
		t.Fatalf("project org keys: %d, %d validated", keys, valid)
	}
	if err := store.MigrateTo(ctx, pool, 67, log); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname LIKE '%_project_org_fkey' OR conname = 'projects_id_org_id_key'`).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("keys after down: %d %v", keys, err)
	}
}
