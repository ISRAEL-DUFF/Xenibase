package orgs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

func roleOf(t *testing.T, db *pgxpool.Pool, org, user uuid.UUID) string {
	t.Helper()
	var r string
	if err := db.QueryRow(context.Background(), `SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2`, org, user).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func projectMemberships(t *testing.T, db *pgxpool.Pool, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM project_members WHERE user_id = $1`, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBillingIsKept: a billing member stays billing, with no project
// access, whether an admin invites their address again, an invitation
// sent before they became billing is accepted, or a project they are a
// member of moves into an organisation where they are billing.
func TestBillingIsKept(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	owner, admin, bill := uuid.New(), uuid.New(), uuid.New()
	from, to := uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'owner@example.com', 'x'), ($2, 'admin@example.com', 'x'), ($3, 'bill@example.com', 'x')`, owner, admin, bill)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'From', 'from', id FROM quota_plans ORDER BY name LIMIT 1`, from)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'To', 'to', id FROM quota_plans ORDER BY name LIMIT 1`, to)
	exec(t, db, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $3, 'owner'), ($2, $3, 'owner'), ($2, $4, 'admin'), ($2, $5, 'billing'), ($1, $5, 'member')`, from, to, owner, admin, bill)
	node, inst := uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', 'fp', '{}')`, node)
	exec(t, db, `INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	shop := projectIn(t, db, from, inst, "shop", nil)
	blog := projectIn(t, db, to, inst, "blog", nil)
	exec(t, db, `INSERT INTO project_members (project_id, user_id, org_id, role) VALUES ($1, $2, $3, 'developer')`, shop.ID, bill, from)
	svc := orgs.New(db, nil, nil, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))

	// An admin inviting the billing member's address again, with a project.
	if _, err := svc.Invite(ctx, orgs.InviteParams{OrgID: &to, Email: "bill@example.com", OrgRole: "member", InvitedBy: admin,
		Projects: []orgs.ProjectRole{{ProjectID: blog.ID, Role: "admin"}}}); !errors.Is(err, orgs.ErrConflict) {
		t.Fatalf("inviting an existing member: %v", err)
	}

	// An invitation from before they became billing, accepted now.
	var inv uuid.UUID
	if err := db.QueryRow(ctx, `INSERT INTO invitations (email, token_hash, kind, org_id, org_role, project_roles, invited_by, expires_at)
		VALUES ('bill@example.com', 'h', 'org', $1, 'member', $2, $3, now() + interval '1 day') RETURNING id`,
		to, `[{"project_id":"`+blog.ID.String()+`","role":"admin"}]`, admin).Scan(&inv); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptID(ctx, inv, bill); err != nil {
		t.Fatal(err)
	}
	if r := roleOf(t, db, to, bill); r != "billing" {
		t.Fatalf("after accepting: %s, want billing", r)
	}
	if n := projectMemberships(t, db, bill); n != 1 { // shop, in From
		t.Fatalf("after accepting, project memberships: %d", n)
	}

	// The project they work on in From moves into To, where they are billing.
	if err := svc.Transfer(ctx, shop, to); err != nil {
		t.Fatal(err)
	}
	if r := roleOf(t, db, to, bill); r != "billing" {
		t.Fatalf("after the transfer: %s, want billing", r)
	}
	if n := projectMemberships(t, db, bill); n != 0 {
		t.Fatalf("a billing member kept a project: %d", n)
	}
	// In From they are still a member.
	if r := roleOf(t, db, from, bill); r != "member" {
		t.Fatalf("in From: %s", r)
	}
}
