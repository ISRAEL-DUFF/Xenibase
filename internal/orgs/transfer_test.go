package orgs_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

func exec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// TestTransferMovesEveryChild: everything that carries the project's
// org_id moves with it, and nothing can be left behind or written under
// another organisation.
func TestTransferMovesEveryChild(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	owner, member := uuid.New(), uuid.New()
	from, to := uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'owner@example.com', 'x'), ($2, 'member@example.com', 'x')`, owner, member)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'From', 'from', id FROM quota_plans ORDER BY name LIMIT 1`, from)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'To', 'to', id FROM quota_plans ORDER BY name LIMIT 1`, to)
	exec(t, db, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $3, 'owner'), ($2, $3, 'owner'), ($1, $4, 'member')`, from, to, owner, member)
	node, inst := uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', 'fp', '{}')`, node)
	exec(t, db, `INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	p, err := store.New(db).InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: from, Name: "app", Slug: "app", DbName: "p_app", OwnerRole: "p_app_owner",
		ScramVerifier: "x", Tier: "shared", InstanceID: inst, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `UPDATE projects SET status = 'active' WHERE id = $1`, p.ID)
	p.Status = "active"

	exec(t, db, `INSERT INTO project_members (project_id, user_id, org_id, role) VALUES ($1, $2, $3, 'read_only')`, p.ID, member, from)
	exec(t, db, `INSERT INTO project_db_users (project_id, user_id, org_id, role_name, scram_verifier, access) VALUES ($1, $2, $3, 'p_app_u_1', 'x', 'read_only')`, p.ID, member, from)
	var saved uuid.UUID
	if err := db.QueryRow(ctx, `INSERT INTO saved_queries (org_id, project_id, owner_id, name, sql) VALUES ($1, $2, $3, 'q', 'select 1') RETURNING id`, from, p.ID, owner).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO saved_query_favorites (query_id, user_id, org_id) VALUES ($1, $2, $3)`, saved, member, from)
	exec(t, db, `INSERT INTO reaped_sessions (org_id, project_id, kind, role_name, duration_s, query) VALUES ($1, $2, 'statement', 'p_app_owner', 600, 'select secret')`, from, p.ID)
	exec(t, db, `INSERT INTO editor_preferences (org_id, project_id, user_id) VALUES ($1, $2, $3)`, from, p.ID, owner)
	exec(t, db, `INSERT INTO dedicated_requests (org_id, project_id, requested_by, profile) VALUES ($1, $2, $3, '{}')`, from, p.ID, owner)

	svc := orgs.New(db, nil, nil, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc.Transfer(ctx, p, to); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"project_members", "project_db_users", "saved_queries", "saved_query_favorites", "reaped_sessions", "editor_preferences", "dedicated_requests"} {
		var left, moved int
		if err := db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE org_id = $1), count(*) FILTER (WHERE org_id = $2) FROM `+table, from, to).Scan(&left, &moved); err != nil {
			t.Fatal(err)
		}
		if left != 0 || moved != 1 {
			t.Errorf("%s after the transfer: %d under the old organisation, %d under the new", table, left, moved)
		}
	}

	// A copy under another organisation than the project's is refused.
	_, err = db.Exec(ctx, `INSERT INTO reaped_sessions (org_id, project_id, kind, role_name, duration_s) VALUES ($1, $2, 'statement', 'x', 1)`, from, p.ID)
	if err == nil || !strings.Contains(err.Error(), "reaped_sessions_project_org_fkey") {
		t.Fatalf("a reaped session under the wrong organisation: %v", err)
	}
}
