package branching

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/authz"
)

// TestResetChecksTheParent: a reset copies the parent again, so the caller
// must still be allowed to branch it, and a full-data branch of a parent
// marked sensitive is refilled only by the parent's project admin.
func TestResetChecksTheParent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dev, admin := h.user(t, authz.OrgMember), h.user(t, authz.OrgMember)
	parent := h.project(t, "Shop", nil)
	branch := h.project(t, "pr-1", &parent)
	for _, p := range []struct {
		id   uuid.UUID
		role string
	}{{dev, authz.ProjectDeveloper}, {admin, authz.ProjectAdmin}} {
		h.member(t, parent, p.id, p.role)
		h.member(t, branch, p.id, p.role)
	}
	reset := func(actor authz.Actor) error {
		t.Helper()
		_, err := h.s.Reset(ctx, ResetParams{BranchID: branch.ID, Source: SourceLive, By: &actor.UserID, Actor: actor})
		if err == nil {
			h.idle(t, branch)
		}
		return err
	}
	if err := reset(session(dev)); err != nil {
		t.Fatalf("a developer on both: %v", err)
	}
	// A token restricted to the parent covers its branches, as for create.
	org := h.org
	token := authz.Actor{Kind: authz.ActorToken, UserID: dev, TokenID: new(uuid.UUID), TokenOrg: &org,
		TokenScopes: []string{authz.ScopeWrite}, TokenProjects: []uuid.UUID{parent.ID}}
	if err := reset(token); err != nil {
		t.Fatalf("a token restricted to the parent: %v", err)
	}
	// One restricted to the branch alone does not reach the parent.
	token.TokenProjects = []uuid.UUID{branch.ID}
	if err := reset(token); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a token restricted to the branch: %v", err)
	}

	// Removed from the parent: no more fresh copies of it.
	if _, err := h.db.Exec(ctx, `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, parent.ID, dev); err != nil {
		t.Fatal(err)
	}
	if err := reset(session(dev)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer removed from the parent: %v", err)
	}
	if h.get(t, branch.ID).Status != "active" {
		t.Fatal("a refused reset changed the branch's status")
	}
	// Read-only on the parent is not enough either.
	h.member(t, parent, dev, authz.ProjectReadOnly)
	if err := reset(session(dev)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only on the parent: %v", err)
	}

	// The parent marked sensitive afterwards: its full-data branch is
	// refilled by its project admin only.
	h.member(t, parent, dev, authz.ProjectDeveloper)
	if _, err := h.db.Exec(ctx, `UPDATE projects SET sensitive_data = true WHERE id = $1`, parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := reset(session(dev)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer refilling a full copy of sensitive data: %v", err)
	}
	if err := reset(session(admin)); err != nil {
		t.Fatalf("the parent's project admin: %v", err)
	}
	// A schema-only branch holds no rows: developers may still reset it.
	if _, err := h.db.Exec(ctx, `UPDATE projects SET branch_schema_only = true WHERE id = $1`, branch.ID); err != nil {
		t.Fatal(err)
	}
	if err := reset(session(dev)); err != nil {
		t.Fatalf("a developer resetting a schema-only branch: %v", err)
	}
}
