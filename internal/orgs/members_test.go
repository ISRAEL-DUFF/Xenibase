package orgs_test

import (
	"context"
	"errors"
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

// projectIn records an active shared project in org (a branch of parent
// when set).
func projectIn(t *testing.T, db *pgxpool.Pool, org, inst uuid.UUID, name string, parent *uuid.UUID) store.Project {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	_, err := store.New(db).InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: id, OrgID: org, Name: name, Slug: name, DbName: "p_" + name, OwnerRole: "p_" + name + "_owner",
		ScramVerifier: "x", Tier: "shared", InstanceID: inst, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `UPDATE projects SET status = 'active', parent_project_id = $2, branch_source = CASE WHEN $2::uuid IS NULL THEN NULL ELSE 'live' END WHERE id = $1`, id, parent)
	p, err := store.New(db).GetProject(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRemovingAParentMemberLeavesItsBranches: a branch holds its parent's
// data and starts with its members, so someone removed from the parent is
// removed from its live branches too, and only from those.
func TestRemovingAParentMemberLeavesItsBranches(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	owner, dev := uuid.New(), uuid.New()
	org := uuid.New()
	exec(t, db, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'owner@example.com', 'x'), ($2, 'dev@example.com', 'x')`, owner, dev)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Org', 'org', id FROM quota_plans ORDER BY name LIMIT 1`, org)
	exec(t, db, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, org, owner, dev)
	node, inst := uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', 'fp', '{}')`, node)
	exec(t, db, `INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	parent := projectIn(t, db, org, inst, "shop", nil)
	branch := projectIn(t, db, org, inst, "shop_pr1", &parent.ID)
	other := projectIn(t, db, org, inst, "blog", nil)
	gone := projectIn(t, db, org, inst, "shop_pr0", &parent.ID)
	exec(t, db, `UPDATE projects SET deleted_at = now(), status = 'deleted' WHERE id = $1`, gone.ID)
	for _, p := range []store.Project{parent, branch, other, gone} {
		exec(t, db, `INSERT INTO project_members (project_id, user_id, org_id, role) VALUES ($1, $2, $3, 'developer')`, p.ID, dev, org)
	}

	svc := orgs.New(db, nil, nil, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc.RemoveProjectMember(ctx, parent, dev); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p    store.Project
		want bool
	}{{parent, false}, {branch, false}, {other, true}, {gone, true}} {
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM project_members WHERE project_id = $1 AND user_id = $2`, c.p.ID, dev).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if (n == 1) != c.want {
			t.Errorf("%s: member %v, want %v", c.p.Name, n == 1, c.want)
		}
	}
	// Removing them from a branch alone leaves the parent alone.
	exec(t, db, `INSERT INTO project_members (project_id, user_id, org_id, role) VALUES ($1, $2, $3, 'developer'), ($4, $2, $3, 'developer')`, parent.ID, dev, org, branch.ID)
	if err := svc.RemoveProjectMember(ctx, branch, dev); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM project_members WHERE project_id = $1 AND user_id = $2`, parent.ID, dev).Scan(&n); err != nil || n != 1 {
		t.Fatalf("removing from a branch removed from the parent: %d %v", n, err)
	}
}

// TestTransferKeepsBranchesWithTheirParent: a branch and its parent share
// an organisation, so neither a branch nor a parent with live branches
// moves; a parent whose branches are gone does.
func TestTransferKeepsBranchesWithTheirParent(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	owner := uuid.New()
	from, to := uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'owner@example.com', 'x')`, owner)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'From', 'from', id FROM quota_plans ORDER BY name LIMIT 1`, from)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'To', 'to', id FROM quota_plans ORDER BY name LIMIT 1`, to)
	exec(t, db, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $3, 'owner'), ($2, $3, 'owner')`, from, to, owner)
	node, inst := uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', 'fp', '{}')`, node)
	exec(t, db, `INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	parent := projectIn(t, db, from, inst, "shop", nil)
	branch := projectIn(t, db, from, inst, "shop_pr1", &parent.ID)

	svc := orgs.New(db, nil, nil, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc.Transfer(ctx, branch, to); !errors.Is(err, orgs.ErrConflict) || !strings.Contains(err.Error(), "detach") {
		t.Fatalf("transfer a branch: %v", err)
	}
	if err := svc.Transfer(ctx, parent, to); !errors.Is(err, orgs.ErrConflict) || !strings.Contains(err.Error(), "branch") {
		t.Fatalf("transfer a parent with a branch: %v", err)
	}
	var orgOf uuid.UUID
	if err := db.QueryRow(ctx, `SELECT org_id FROM projects WHERE id = $1`, parent.ID).Scan(&orgOf); err != nil || orgOf != from {
		t.Fatalf("the parent moved: %v %v", orgOf, err)
	}
	// Once its branch is deleted, the parent moves.
	exec(t, db, `UPDATE projects SET deleted_at = now(), status = 'deleted' WHERE id = $1`, branch.ID)
	if err := svc.Transfer(ctx, parent, to); err != nil {
		t.Fatalf("transfer a parent whose branches are gone: %v", err)
	}
}

// TestNobodyInvitesThemselves: an invitation to the inviter's own address
// is refused (only a platform admin under break-glass, who is not a member,
// could want one, and their access must end with the session).
func TestNobodyInvitesThemselves(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	admin, org := uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'Root@example.com', 'x')`, admin)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Org', 'org', id FROM quota_plans ORDER BY name LIMIT 1`, org)
	svc := orgs.New(db, nil, nil, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := svc.Invite(ctx, orgs.InviteParams{OrgID: &org, Email: "root@example.com", OrgRole: "admin", InvitedBy: admin}); !errors.Is(err, orgs.ErrForbidden) {
		t.Fatalf("inviting oneself: %v", err)
	}
	if _, err := svc.Invite(ctx, orgs.InviteParams{OrgID: &org, Email: "someone@example.com", OrgRole: "admin", InvitedBy: admin}); err != nil {
		t.Fatalf("inviting someone else: %v", err)
	}
}

// TestLeavingUnverifiesTheirBillingContact: a billing contact counts as
// verified while it is a member's own address, so when the member leaves
// it stops routing support tickets to the organisation until confirmed by
// link, and the same address in another organisation is left alone.
func TestLeavingUnverifiesTheirBillingContact(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	owner, dev := uuid.New(), uuid.New()
	org, other := uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO users (id, email, password_hash, email_verified_at) VALUES ($1, 'owner@example.com', 'x', now()), ($2, 'dev@example.com', 'x', now())`, owner, dev)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Org', 'org', id FROM quota_plans ORDER BY name LIMIT 1`, org)
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Other', 'other', id FROM quota_plans ORDER BY name LIMIT 1`, other)
	exec(t, db, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member'), ($4, $3, 'owner')`, org, owner, dev, other)
	exec(t, db, `INSERT INTO billing_contacts (org_id, email, verified_at) VALUES ($1, 'dev@example.com', now()), ($2, 'dev@example.com', now())`, org, other)

	svc := orgs.New(db, nil, nil, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := svc.Leave(ctx, org, dev); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		org  uuid.UUID
		want bool
	}{{org, false}, {other, true}} {
		var verified bool
		if err := db.QueryRow(ctx, `SELECT verified_at IS NOT NULL FROM billing_contacts WHERE org_id = $1 AND email = 'dev@example.com'`, c.org).Scan(&verified); err != nil {
			t.Fatal(err)
		}
		if verified != c.want {
			t.Errorf("org %s: contact verified %v, want %v", c.org, verified, c.want)
		}
	}
}
