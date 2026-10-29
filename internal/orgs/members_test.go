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
	p, err := store.New(db).InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: id, OrgID: org, Name: name, Slug: name, DbName: "p_" + name, OwnerRole: "p_" + name + "_owner",
		ScramVerifier: "x", Tier: "shared", InstanceID: inst, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, `UPDATE projects SET status = 'active', parent_project_id = $2, branch_source = CASE WHEN $2::uuid IS NULL THEN NULL ELSE 'live' END WHERE id = $1`, id, parent)
	p, err = store.New(db).GetProject(ctx, id)
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
