package branching

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/services"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tenancy"
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

// TestDetachIsANewProject: detaching makes a standalone project, so it
// gets the checks a new project gets (org.create_project and the projects
// quota), and it waits for the branch to be idle and active.
func TestDetachIsANewProject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ten := tenancy.New(h.db, h.s.projects, nil, tenancy.Config{}, quiet)
	dev, admin := h.user(t, authz.OrgMember), h.user(t, authz.OrgAdmin)
	parent := h.project(t, "Shop", nil)
	branch := h.project(t, "pr-1", &parent)
	h.member(t, parent, dev, authz.ProjectDeveloper)
	h.member(t, branch, dev, authz.ProjectAdmin)
	detach := func(actor authz.Actor) error {
		t.Helper()
		_, err := h.s.Detach(ctx, DetachParams{ID: branch.ID, Actor: actor, CheckCreateProject: ten.CheckCreateProject})
		return err
	}
	stillBranch := func(what string) {
		t.Helper()
		if h.get(t, branch.ID).ParentProjectID == nil {
			t.Fatalf("%s: the branch was detached", what)
		}
	}

	// Members may not create projects here: a developer can't detach.
	if _, err := h.db.Exec(ctx, `UPDATE organizations SET settings = '{"members_can_create_projects": false}' WHERE id = $1`, h.org); err != nil {
		t.Fatal(err)
	}
	if err := detach(session(dev)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a developer who may not create projects: %v", err)
	}
	stillBranch("a developer who may not create projects")

	// The projects quota counts the parent; the detached branch would be
	// one more.
	if _, err := h.db.Exec(ctx, `UPDATE organizations SET limit_overrides = '{"projects": 1}' WHERE id = $1`, h.org); err != nil {
		t.Fatal(err)
	}
	var qe *tenancy.QuotaError
	if err := detach(session(admin)); !errors.As(err, &qe) || qe.Limit != store.LimitProjects {
		t.Fatalf("detach beyond the projects quota: %v", err)
	}
	stillBranch("beyond the quota")
	if _, err := h.db.Exec(ctx, `UPDATE organizations SET limit_overrides = '{"projects": 2}' WHERE id = $1`, h.org); err != nil {
		t.Fatal(err)
	}

	// Not while a reset is in flight, nor while the branch isn't active.
	if _, err := h.s.Reset(ctx, ResetParams{BranchID: branch.ID, Source: SourceLive, Actor: session(admin)}); err != nil {
		t.Fatal(err)
	}
	if err := detach(session(admin)); !errors.Is(err, provision.ErrConflict) {
		t.Fatalf("detach during a reset: %v", err)
	}
	stillBranch("during a reset")
	if _, err := h.db.Exec(ctx, `DELETE FROM operations WHERE project_id = $1`, branch.ID); err != nil {
		t.Fatal(err)
	}
	if err := detach(session(admin)); !errors.Is(err, provision.ErrConflict) {
		t.Fatalf("detach of a restoring branch: %v", err)
	}
	h.idle(t, branch)

	p, err := h.s.Detach(ctx, DetachParams{ID: branch.ID, Actor: session(admin), CheckCreateProject: ten.CheckCreateProject})
	if err != nil || p.ParentProjectID != nil || p.ExpiresAt != nil {
		t.Fatalf("detach: %+v %v", p, err)
	}
	if _, err := h.s.Detach(ctx, DetachParams{ID: branch.ID, Actor: session(admin)}); !errors.Is(err, ErrNotBranch) {
		t.Fatalf("detach a standalone project: %v", err)
	}
}

// TestBranchInAnotherOrganisationIsNotFilled: a branch whose parent is in
// another organisation (split by a transfer before transfers refused it)
// is neither reset nor filled from the other organisation's data.
func TestBranchInAnotherOrganisationIsNotFilled(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner := h.user(t, authz.OrgOwner)
	parent := h.project(t, "Shop", nil)
	branch := h.project(t, "pr-1", &parent)
	var other uuid.UUID
	if err := h.db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id, settings)
		SELECT 'Other', 'other', plan_id, '{}' FROM organizations WHERE id = $1 RETURNING id`, h.org).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if err := store.New(h.db).InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: other, UserID: owner, Role: authz.OrgOwner}); err != nil {
		t.Fatal(err)
	}
	if err := store.New(h.db).SetProjectOrg(ctx, store.SetProjectOrgParams{ID: branch.ID, OrgID: h.org, NewOrgID: other}); err != nil {
		t.Fatal(err)
	}
	branch = h.get(t, branch.ID)
	if _, err := h.s.Reset(ctx, ResetParams{BranchID: branch.ID, Source: SourceLive, Actor: session(owner)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reset across organisations: %v", err)
	}
	// An operation queued before: the worker refuses it for good.
	err := h.s.fill(ctx, branch, fillParams{Parent: parent.ID, Source: SourceLive}, nil)
	if !jobs.IsPermanent(err) || !strings.Contains(err.Error(), "another organisation") {
		t.Fatalf("fill across organisations: %v", err)
	}
}

// TestSweepChecksTheLockedRow: the sweep deletes from a list it read
// earlier, so each delete looks at the locked row again: a branch extended
// or detached since is kept, and the final backup follows the row as it is.
func TestSweepChecksTheLockedRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	q := store.New(h.db)
	parent := h.project(t, "Shop", nil)
	extended, detached, backedUp, expired := h.project(t, "a", &parent), h.project(t, "b", &parent), h.project(t, "c", &parent), h.project(t, "d", &parent)
	past := h.now.Add(-time.Minute)
	if _, err := h.db.Exec(ctx, `UPDATE projects SET expires_at = $1 WHERE parent_project_id = $2`, past, parent.ID); err != nil {
		t.Fatal(err)
	}
	list, err := q.ExpiredBranches(ctx, h.now)
	if err != nil || len(list) != 4 {
		t.Fatalf("expired: %d %v", len(list), err)
	}
	// Between the list and the deletes, users act.
	if _, err := h.s.SetExpiry(ctx, extended.ID, ptr(h.now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := q.DetachBranch(ctx, detached.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.SetBackups(ctx, backedUp.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, b := range list {
		op, err := h.s.expire(ctx, b.ID, h.now)
		switch b.ID {
		case extended.ID, detached.ID:
			if !errors.Is(err, provision.ErrConflict) {
				t.Errorf("%s: %v", b.Name, err)
			}
			if p := h.get(t, b.ID); p.Status != provision.StatusActive {
				t.Errorf("%s was deleted: %s", b.Name, p.Status)
			}
		case backedUp.ID, expired.ID:
			if err != nil {
				t.Fatalf("%s: %v", b.Name, err)
			}
			var params struct {
				Skip bool `json:"skip_final_backup"`
			}
			if err := json.Unmarshal(op.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params.Skip != (b.ID == expired.ID) {
				t.Errorf("%s: skip_final_backup %v", b.Name, params.Skip)
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }

// fakeAPI records what runCreate and runReset ask of backend services.
type fakeAPI struct {
	enabled int
	files   []services.BranchFiles
}

func (f *fakeAPI) PrepareBranch(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*services.BranchAPI, error) {
	return nil, nil
}

func (f *fakeAPI) EnableBranch(context.Context, store.Project, *jobs.StepLogger) error {
	f.enabled++
	return nil
}

func (f *fakeAPI) BranchFiles(_ context.Context, _ store.Project, _ uuid.UUID, bf services.BranchFiles, _ *jobs.StepLogger) error {
	f.files = append(f.files, bf)
	return nil
}

// TestCreateRetryAfterPublishKeepsTheBranch: a create that failed after
// Publish is retried on a branch that is active, routed and in use; the
// retry must not recreate its database under its clients, only finish.
func TestCreateRetryAfterPublishKeepsTheBranch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	api := &fakeAPI{}
	h.s.API = api
	owner := h.user(t, authz.OrgOwner)
	parent := h.project(t, "Shop", nil)
	c, _, err := h.s.Create(ctx, CreateParams{ParentID: parent.ID, Name: "pr-1", CopyFiles: true, CreatedBy: &owner},
		Resolved{Parent: parent, Source: SourceLive})
	if err != nil {
		t.Fatal(err)
	}
	b := c.Project
	t.Cleanup(func() {
		_, _ = h.cluster.Exec(context.Background(), "DROP DATABASE IF EXISTS "+provision.Ident(b.DbName)+" WITH (FORCE)")
		_, _ = h.cluster.Exec(context.Background(), "DROP OWNED BY "+provision.Ident(b.OwnerRole))
		_, _ = h.cluster.Exec(context.Background(), "DROP ROLE IF EXISTS "+provision.Ident(b.OwnerRole))
	})
	op, log := h.claim(t, c.Operation)
	// The first attempt got as far as Publish: the branch's database holds
	// its copy, and the branch is active.
	if err := h.s.projects.Prepare(ctx, b, log); err != nil {
		t.Fatal(err)
	}
	conn, err := h.s.projects.AdminConn(ctx, b.InstanceID, b.DbName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE items (id int)`); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	if err := store.New(h.db).SetProjectStatus(ctx, store.SetProjectStatusParams{ID: b.ID, Status: provision.StatusActive}); err != nil {
		t.Fatal(err)
	}

	op.Attempts = 2
	if err := h.s.runCreate(ctx, op, log); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	conn, err = h.s.projects.AdminConn(ctx, b.InstanceID, b.DbName)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var kept bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('items') IS NOT NULL`).Scan(&kept); err != nil || !kept {
		t.Fatalf("the retry recreated the active branch's database: %v %v", kept, err)
	}
	if api.enabled != 0 || len(api.files) != 1 || !api.files[0].Copy {
		t.Fatalf("the retry's backend services: enabled %d times, files %+v", api.enabled, api.files)
	}
}

// TestResetCopiesFilesAgain: a branch made with copy_files remembers it,
// and its resets copy the parent's files again.
func TestResetCopiesFilesAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner := h.user(t, authz.OrgOwner)
	parent := h.project(t, "Shop", nil)
	for _, copyFiles := range []bool{true, false} {
		c, _, err := h.s.Create(ctx, CreateParams{ParentID: parent.ID, Name: "pr", CopyFiles: copyFiles, CreatedBy: &owner},
			Resolved{Parent: parent, Source: SourceLive})
		if err != nil {
			t.Fatal(err)
		}
		if got := h.get(t, c.Project.ID).BranchCopyFiles; got != copyFiles {
			t.Fatalf("copy_files %v: the branch remembers %v", copyFiles, got)
		}
		h.idle(t, c.Project)
		op, err := h.s.Reset(ctx, ResetParams{BranchID: c.Project.ID, Source: SourceLive, Actor: session(owner)})
		if err != nil {
			t.Fatal(err)
		}
		params, err := opFill(op)
		if err != nil || params.CopyFiles != copyFiles {
			t.Fatalf("copy_files %v: the reset's params %+v %v", copyFiles, params, err)
		}
		if _, err := h.db.Exec(ctx, `UPDATE projects SET deleted_at = now(), status = 'deleted' WHERE id = $1`, c.Project.ID); err != nil {
			t.Fatal(err)
		}
	}
}
