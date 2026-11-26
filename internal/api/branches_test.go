package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/branching"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// branchWorld is a Server with branching and tenancy against a throwaway
// metadata database, an organisation with its owner, and a parent project;
// nothing runs on a cluster.
type branchWorld struct {
	s      *Server
	db     *pgxpool.Pool
	org    uuid.UUID
	owner  uuid.UUID
	inst   uuid.UUID
	parent store.Project
}

func newBranchWorld(t *testing.T) *branchWorld {
	t.Helper()
	ctx := context.Background()
	db := storetest.New(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	key, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(key)
	pm, err := pooler.NewManager(t.TempDir(), 0o644, db, nil, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	ps := provision.NewService(db, kr, pm, provision.Config{HomeRegion: "eu-central"}, log)
	w := &branchWorld{db: db, owner: uuid.New(), inst: uuid.New()}
	w.s = &Server{log: log, db: db, projects: ps,
		branches: branching.New(db, ps, nil, nil, nil, branching.Config{}, log),
		tenancy:  tenancy.New(db, ps, nil, tenancy.Config{}, log)}
	node := uuid.New()
	for _, sql := range []string{
		`INSERT INTO users (id, email, password_hash) VALUES ('` + w.owner.String() + `', 'owner@example.com', 'x')`,
		`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ('` + node.String() + `', 'n1', '10.0.0.1', 'both', 'fp', '{}')`,
		`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ('` + w.inst.String() + `', '` + node.String() + `', 'shared', 18, 5432, 'running')`,
	} {
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id, settings)
		SELECT 'Org', 'org', id, '{}' FROM quota_plans ORDER BY created_at LIMIT 1 RETURNING id`).Scan(&w.org); err != nil {
		t.Fatal(err)
	}
	if err := store.New(db).InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: w.org, UserID: w.owner, Role: authz.OrgOwner}); err != nil {
		t.Fatal(err)
	}
	w.parent = w.project(t, "shop", nil)
	return w
}

// project records an active project (a branch of parent when set).
func (w *branchWorld) project(t *testing.T, name string, parent *store.Project) store.Project {
	t.Helper()
	ctx := context.Background()
	q := store.New(w.db)
	p, err := q.InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: w.org, Name: name, Slug: name, DbName: "p_" + name, OwnerRole: "p_" + name + "_owner",
		ScramVerifier: "x", Tier: provision.TierShared, InstanceID: w.inst, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if parent != nil {
		src, schemaOnly := branching.SourceLive, false
		if err := q.SetProjectBranch(ctx, store.SetProjectBranchParams{ID: p.ID, ParentProjectID: &parent.ID, BranchSource: &src, BranchSchemaOnly: &schemaOnly}); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: provision.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if p, err = q.GetProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	return p
}

// call runs a branch handler for the organisation's owner, as the guard
// would have resolved the request.
func (w *branchWorld) call(t *testing.T, p store.Project, h func(http.ResponseWriter, *http.Request, gen.ProjectID)) (int, gen.Error) {
	t.Helper()
	acc := access{Actor: authz.Actor{Kind: authz.ActorSession, UserID: w.owner}, OrgID: p.OrgID, ProjectID: p.ID,
		OrgRole: authz.OrgOwner, ProjectRole: authz.ProjectAdmin}
	if p.ParentProjectID != nil {
		acc.ParentID = *p.ParentProjectID
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	r = r.WithContext(context.WithValue(r.Context(), keyAccess, acc))
	rec := httptest.NewRecorder()
	h(rec, r, p.ID)
	var e gen.Error
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return rec.Code, e
}

// TestResetBranchCountsTowardsOperationsInFlight: a reset is a copy of the
// parent, so it is refused beyond the plan's operations in flight, as a
// new branch is.
func TestResetBranchCountsTowardsOperationsInFlight(t *testing.T) {
	w := newBranchWorld(t)
	if _, err := w.db.Exec(context.Background(), `UPDATE organizations SET limit_overrides = '{"operations_in_flight": 1}' WHERE id = $1`, w.org); err != nil {
		t.Fatal(err)
	}
	a, b := w.project(t, "pr1", &w.parent), w.project(t, "pr2", &w.parent)
	if code, e := w.call(t, a, w.s.ResetBranch); code != http.StatusAccepted {
		t.Fatalf("the first reset: %d %+v", code, e)
	}
	code, e := w.call(t, b, w.s.ResetBranch)
	if code != http.StatusConflict || e.Code != "quota_exceeded" || e.Quota == nil || e.Quota.Limit != store.LimitOperationsInFlight {
		t.Fatalf("a reset beyond operations in flight: %d %+v", code, e)
	}
}

// TestDetachBranchAnswersQuota: a detach beyond the projects quota is the
// quota error a new project gets.
func TestDetachBranchAnswersQuota(t *testing.T) {
	w := newBranchWorld(t)
	if _, err := w.db.Exec(context.Background(), `UPDATE organizations SET limit_overrides = '{"projects": 1}' WHERE id = $1`, w.org); err != nil {
		t.Fatal(err)
	}
	b := w.project(t, "pr1", &w.parent)
	code, e := w.call(t, b, w.s.DetachBranch)
	if code != http.StatusConflict || e.Code != "quota_exceeded" || e.Quota == nil || e.Quota.Limit != store.LimitProjects {
		t.Fatalf("detach beyond the projects quota: %d %+v", code, e)
	}
}
