package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

func allRoutes(t *testing.T) []string {
	t.Helper()
	h := NewHandler(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), InsecureNoAuth: true,
		UI: fstest.MapFS{"index.html": {Data: []byte("app")}}, UIIndex: "index.html"})
	var out []string
	if err := chi.Walk(h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, method+" "+route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestEveryRouteDeclaresAnAction is V2 §2.6's CI check: no route without
// an authorization rule (and no rule for a route that is gone).
func TestEveryRouteDeclaresAnAction(t *testing.T) {
	routes := allRoutes(t)
	if len(routes) < 100 {
		t.Fatalf("only %d routes found", len(routes))
	}
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r] = true
		rl, ok := routeRules[r]
		if !ok {
			t.Errorf("%s has no entry in routeRules", r)
			continue
		}
		if rl.scope != scopePublic && rl.action == "" {
			t.Errorf("%s declares no action", r)
		}
		if strings.Contains(r, "{id}") && strings.HasPrefix(strings.SplitN(r, " ", 2)[1], "/api/v1/projects/{id}") && rl.scope != scopeProject {
			t.Errorf("%s is a project route but resolves its resource as scope %d", r, rl.scope)
		}
		if rl.scope == scopeProject && !authz.IsProjectAction(rl.action) {
			t.Errorf("%s: project scope with the non-project action %s", r, rl.action)
		}
	}
	for r := range routeRules {
		if !seen[r] {
			t.Errorf("routeRules has %s, which is not a route", r)
		}
	}
}

// ---- The permission matrix --------------------------------------------------------

// Actors in the matrix world. Everyone but outsider and platform is in org A.
const (
	rOwner     = "org_owner"
	rAdmin     = "org_admin"
	rProjAdmin = "project_admin"
	rDev       = "developer"
	rReadOnly  = "read_only"
	rMember    = "member_without_project"
	rBilling   = "billing_member"
	rOutsider  = "other_org_owner"
	rPlatform  = "platform_admin"
	rAnonymous = "anonymous"
)

var matrixActors = []string{rOwner, rAdmin, rProjAdmin, rDev, rReadOnly, rMember, rBilling, rOutsider, rPlatform, rAnonymous}

// expected is the spec's matrix (V2 §2.3, §2.4), written out
// independently of authz: who may perform each action on org A and its
// project P.
var expected = map[authz.Action][]string{
	authz.ProjectView:        {rOwner, rAdmin, rProjAdmin, rDev, rReadOnly},
	authz.ProjectCredentials: {rOwner, rAdmin, rProjAdmin, rDev, rReadOnly},
	authz.ConsoleRead:        {rOwner, rAdmin, rProjAdmin, rDev, rReadOnly},
	authz.ConsoleWrite:       {rOwner, rAdmin, rProjAdmin, rDev},
	authz.TableEdit:          {rOwner, rAdmin, rProjAdmin, rDev},
	authz.BackupCreate:       {rOwner, rAdmin, rProjAdmin, rDev},
	authz.RestoreInPlace:     {rOwner, rAdmin, rProjAdmin},
	authz.BackupStorage:      {rOwner, rAdmin, rProjAdmin},
	authz.BranchManage:       {rOwner, rAdmin, rProjAdmin, rDev},
	authz.AutomationManage:   {rOwner, rAdmin, rProjAdmin, rDev},
	authz.ProjectSettings:    {rOwner, rAdmin, rProjAdmin},
	authz.ServicesManage:     {rOwner, rAdmin, rProjAdmin},
	authz.ServicesLogs:       {rOwner, rAdmin, rProjAdmin, rDev},
	authz.AuthUsers:          {rOwner, rAdmin, rProjAdmin, rDev},
	authz.AuthUsersManage:    {rOwner, rAdmin, rProjAdmin, rDev},
	authz.FilesView:          {rOwner, rAdmin, rProjAdmin, rDev},
	authz.FilesManage:        {rOwner, rAdmin, rProjAdmin, rDev},
	authz.RealtimeView:       {rOwner, rAdmin, rProjAdmin, rDev},
	authz.RealtimeManage:     {rOwner, rAdmin, rProjAdmin, rDev},
	authz.ProjectMembers:     {rOwner, rAdmin, rProjAdmin},
	authz.ProjectPromote:     {rOwner, rAdmin, rProjAdmin},
	authz.ProjectDelete:      {rOwner, rAdmin, rProjAdmin},
	authz.ProjectAudit:       {rOwner, rAdmin, rProjAdmin},
	authz.OrgView:            {rOwner, rAdmin, rProjAdmin, rDev, rReadOnly, rMember, rBilling},
	authz.OrgCreateProject:   {rOwner, rAdmin, rProjAdmin, rDev, rReadOnly, rMember},
	authz.OrgManage:          {rOwner, rAdmin},
	authz.OrgAudit:           {rOwner, rAdmin},
	authz.OrgOwnerOnly:       {rOwner},
	authz.OrgBillingManage:   {rOwner, rBilling}, // V3 §3.2: not admins
	authz.ProjectExport:      {rOwner},
	authz.ProjectResidency:   {rOwner},
	authz.ProjectManageRoles: {rOwner},
	authz.PlatformManage:     {rPlatform},
}

// seesProject is who may know project P exists.
var seesProject = []string{rOwner, rAdmin, rProjAdmin, rDev, rReadOnly}

// seesOrg is who may know org A exists.
var seesOrg = []string{rOwner, rAdmin, rProjAdmin, rDev, rReadOnly, rMember, rBilling}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type matrixWorld struct {
	tokens       map[string]string
	users        map[string]uuid.UUID
	orgA, orgB   uuid.UUID
	project      uuid.UUID
	backup       uuid.UUID
	operation    uuid.UUID
	platformOp   uuid.UUID
	invitation   uuid.UUID
	authzHandler http.Handler
	pool         *pgxpool.Pool
	log          *slog.Logger
}

// stubServer answers 501 for every route: a request that reaches it got
// past the guard.
type stubServer struct{ gen.Unimplemented }

func newMatrixWorld(t *testing.T) *matrixWorld {
	t.Helper()
	ctx := context.Background()
	pool := storetest.New(t)
	key, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(key)
	clk := &fakeClock{t: time.Now()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.NewService(pool, kr, auth.Config{Now: clk.Now, ReauthWindow: time.Hour}, "code", log)
	if err := svc.EnsureTerms(ctx); err != nil {
		t.Fatal(err)
	}
	orgSvc := orgs.New(pool, svc, nil, nil, "", log)
	svc.SetHooks(orgSvc.Hooks())
	w := &matrixWorld{tokens: map[string]string{}, users: map[string]uuid.UUID{}, pool: pool, log: log}

	// The platform admin, through setup.
	enr, err := svc.BeginSetup(ctx, "code", "platform@example.com", ownerPassword)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := auth.TOTPCode(enr.Secret, clk.Now())
	in, err := svc.CompleteSetup(ctx, enr.Token, code, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	w.tokens[rPlatform], w.users[rPlatform] = in.Token, in.Session.UserID
	clk.step()

	// Everyone else: accounts with a verified address, signed in once.
	for _, r := range []string{rOwner, rAdmin, rProjAdmin, rDev, rReadOnly, rMember, rBilling, rOutsider} {
		addr := strings.ReplaceAll(r, "_", "-") + "@example.com"
		var u store.User
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			u, err = svc.CreateVerifiedUser(ctx, tx, addr, ownerPassword, r, 1, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		ch, err := svc.Login(ctx, addr, ownerPassword)
		if err != nil {
			t.Fatal(err)
		}
		signed, err := svc.CompleteLogin(ctx, ch.Token, "", nil, "") // no two-factor: the password was enough
		if err != nil {
			t.Fatal(err)
		}
		w.tokens[r], w.users[r] = signed.Token, u.ID
	}

	q := store.New(pool)
	oa, err := orgSvc.Create(ctx, w.users[rOwner], "Org A")
	if err != nil {
		t.Fatal(err)
	}
	ob, err := orgSvc.Create(ctx, w.users[rOutsider], "Org B")
	if err != nil {
		t.Fatal(err)
	}
	w.orgA, w.orgB = oa.ID, ob.ID
	for r, role := range map[string]string{rAdmin: "admin", rProjAdmin: "member", rDev: "member", rReadOnly: "member", rMember: "member", rBilling: "billing"} {
		if err := q.InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: w.orgA, UserID: w.users[r], Role: role}); err != nil {
			t.Fatal(err)
		}
	}
	node, inst := uuid.New(), uuid.New()
	for _, stmt := range []string{
		`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ('` + node.String() + `', 'n1', '10.0.0.1', 'both', 'fp', '{}')`,
		`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ('` + inst.String() + `', '` + node.String() + `', 'shared', 18, 5432, 'running')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	p, err := q.InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central",
		ID:     uuid.New(), OrgID: w.orgA, Name: "P", Slug: "p", DbName: "p_abcd", OwnerRole: "p_abcd_owner",
		ScramVerifier: "x", Tier: "shared", InstanceID: inst, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	w.project = p.ID
	for r, role := range map[string]string{rProjAdmin: "admin", rDev: "developer", rReadOnly: "read_only"} {
		if err := q.UpsertProjectMember(ctx, store.UpsertProjectMemberParams{ProjectID: p.ID, UserID: w.users[r], OrgID: w.orgA, Role: role}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO backups (project_id, kind, object_key, started_at, status) VALUES ($1, 'logical', 'k', now(), 'succeeded') RETURNING id`, p.ID).Scan(&w.backup); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO operations (kind, project_id) VALUES ('noop', $1) RETURNING id`, p.ID).Scan(&w.operation); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO operations (kind) VALUES ('noop') RETURNING id`).Scan(&w.platformOp); err != nil {
		t.Fatal(err)
	}
	w.invitation = uuid.New()

	// The guard in front of a server that answers 501 everywhere.
	s := &Server{log: log, db: pool, auth: svc}
	r := chi.NewRouter()
	r.Use(s.guard)
	gen.HandlerWithOptions(stubServer{}, gen.ChiServerOptions{BaseRouter: r})
	w.authzHandler = r
	return w
}

// path fills a route pattern's parameters with the world's resources.
func (w *matrixWorld) path(pattern string, platformOp bool) string {
	id := w.project.String()
	switch {
	case strings.HasPrefix(pattern, "/api/v1/backups/{id}"):
		id = w.backup.String()
	case strings.HasPrefix(pattern, "/api/v1/operations/{id}"):
		id = w.operation.String()
		if platformOp {
			id = w.platformOp.String()
		}
	case strings.HasPrefix(pattern, "/api/v1/nodes/{id}"):
		id = uuid.New().String()
	}
	p := strings.NewReplacer(
		"{id}", id, "{org}", w.orgA.String(), "{user}", w.users[rMember].String(),
		"{invitation_id}", w.invitation.String(), "{session_id}", "abc", "{schema}", "public", "{table}", "t",
		"{plan_id}", uuid.NewString(), "{request_id}", uuid.NewString(), "{token_id}", uuid.NewString(), "{target_id}", uuid.NewString(),
		"{webhook_id}", uuid.NewString(), "{job_id}", uuid.NewString(),
		"{user_code}", "BCDF-GHJK", "{email}", "ap@example.com", "{version}", "1", "{invoice_id}", uuid.NewString(), "{method_id}", uuid.NewString(), "{payment_id}", uuid.NewString(), "{event_id}", "1",
	).Replace(pattern)
	for _, m := range []string{"GET", "POST"} {
		if rl := routeRules[m+" "+pattern]; rl.scope == scopeOrgQuery {
			p += "?org=" + w.orgA.String()
			break
		}
	}
	return p
}

func (w *matrixWorld) call(t *testing.T, actor, method, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if tok, ok := w.tokens[actor]; ok {
		req.AddCookie(&http.Cookie{Name: sessionName, Value: tok})
	}
	req.AddCookie(&http.Cookie{Name: csrfName, Value: strings.Repeat("c", 40)})
	req.Header.Set(csrfHeader, strings.Repeat("c", 40))
	rec := httptest.NewRecorder()
	w.authzHandler.ServeHTTP(rec, req)
	return rec.Code, rec.Header().Get("X-Xenibase-Authz")
}

// TestPermissionMatrix checks every route for every kind of actor against
// the spec's matrix (V2 §2.3, §2.4, §2.6): 404 for resources the actor
// may not know about, 403 for what they see but may not do, and through
// to the handler otherwise.
func TestPermissionMatrix(t *testing.T) {
	w := newMatrixWorld(t)
	for _, route := range allRoutes(t) {
		method, pattern, _ := strings.Cut(route, " ")
		rl := routeRules[route]
		if pattern == "/metrics" {
			continue // its own bearer-token check
		}
		for _, actor := range matrixActors {
			want := "allow"
			switch {
			case rl.scope == scopePublic:
			case actor == rAnonymous:
				want = "unauthenticated"
			case rl.scope == scopeSelf, rl.scope == scopeBody:
				// Body routes check the organisation in the handler.
			case rl.scope == scopePlatform:
				if actor != rPlatform {
					want = "denied"
				}
			case rl.scope == scopeOrgPath || rl.scope == scopeOrgQuery:
				if !has(seesOrg, actor) {
					want = "hidden"
				} else if !has(expected[rl.action], actor) {
					want = "denied"
				}
			default: // project, backup, operation
				if !has(seesProject, actor) {
					want = "hidden"
				} else if !has(expected[rl.action], actor) {
					want = "denied"
				}
			}
			status, hdr := w.call(t, actor, method, w.path(pattern, false))
			got := "allow"
			switch {
			case status == http.StatusUnauthorized:
				got = "unauthenticated"
			case hdr != "":
				got = hdr
			case status == http.StatusForbidden || status == http.StatusNotFound:
				// Refused before the guard decided (CSRF, re-auth, terms)?
				got = fmt.Sprintf("refused-%d", status)
			}
			if got != want {
				t.Errorf("%s as %s: got %s (HTTP %d), want %s", route, actor, got, status, want)
			}
		}
	}

	// Platform operations: the platform admin's alone, and invisible to
	// everyone else.
	for _, actor := range []string{rOwner, rOutsider, rPlatform} {
		_, hdr := w.call(t, actor, "GET", w.path("/api/v1/operations/{id}", true))
		if (actor == rPlatform) != (hdr == "") {
			t.Errorf("platform operation as %s: authz header %q", actor, hdr)
		}
	}
	// Another organisation's ids look like ids that do not exist.
	_, hdrForeign := w.call(t, rOutsider, "GET", "/api/v1/projects/"+w.project.String())
	_, hdrMissing := w.call(t, rOutsider, "GET", "/api/v1/projects/"+uuid.New().String())
	if hdrForeign != "hidden" || hdrMissing != "hidden" {
		t.Errorf("foreign %q vs missing %q", hdrForeign, hdrMissing)
	}
}

// TestMembersCanCreateProjectsSetting covers the one org action that
// depends on a setting.
func TestMembersCanCreateProjectsSetting(t *testing.T) {
	w := newMatrixWorld(t)
	ctx := context.Background()
	check := func(actor string) bool {
		d, err := authz.Can(ctx, store.New(w.pool), authz.Actor{Kind: authz.ActorSession, UserID: w.users[actor]}, authz.OrgCreateProject, authz.Resource{OrgID: w.orgA})
		if err != nil {
			t.Fatal(err)
		}
		return d.Allowed
	}
	if !check(rMember) || !check(rOwner) {
		t.Fatal("members create projects by default")
	}
	off := false
	if _, err := orgs.New(w.pool, nil, nil, nil, "", w.log).Update(ctx, w.orgA, orgs.Patch{MembersCanCreateProjects: &off}); err != nil {
		t.Fatal(err)
	}
	if check(rMember) || !check(rAdmin) {
		t.Fatal("with the setting off, only owners and admins create projects")
	}
	if check(rOutsider) {
		t.Fatal("an outsider created a project in org A")
	}
}

// specMatrix restates V2 §2.3/§2.4 row by row: which routes each action of
// the spec's matrix covers. It is kept apart from routeRules on purpose, so
// a route declared with the wrong action fails here.
var specMatrix = map[authz.Action][]string{
	// "View project, metrics, operations"
	authz.ProjectView: {
		// V3 §4.2 "Owners can also resume from the dashboard": anyone who could connect wakes it anyway
		"POST /api/v1/projects/{id}/resume",
		"GET /api/v1/projects/{id}", "GET /api/v1/projects/{id}/metrics", "GET /api/v1/projects/{id}/extensions",
		"GET /api/v1/projects/{id}/members", "GET /api/v1/operations/{id}", "GET /api/v1/operations/{id}/stream",
		// §10.4: storage against the limit, and the reaper's log
		"GET /api/v1/projects/{id}/storage", "GET /api/v1/projects/{id}/reaped",
		// V2 §6: where the project's backups go (choosing is BackupStorage)
		"GET /api/v1/projects/{id}/storage-target",
		// V2 §8 Project → Branches
		"GET /api/v1/projects/{id}/branches",
		// V3 §2.3: a project's moves between instances
		"GET /api/v1/projects/{id}/ha", "GET /api/v1/projects/{id}/replicas", "GET /api/v1/projects/{id}/moves",
		// V4 §2.2 the API URL and keys (publishable keys are meant to be seen)
		"GET /api/v1/projects/{id}/services",
	},
	// V4 §2.4 enabling backend services, keys, gateway settings
	authz.ServicesManage: {
		"POST /api/v1/projects/{id}/services", "PATCH /api/v1/projects/{id}/services", "DELETE /api/v1/projects/{id}/services",
		"POST /api/v1/projects/{id}/services/keys", "DELETE /api/v1/projects/{id}/services/keys/{key_id}",
		// V4 §4 auth settings, templates, SMTP and signing keys
		"PATCH /api/v1/projects/{id}/auth/config", "POST /api/v1/projects/{id}/auth/smtp/test",
		"POST /api/v1/projects/{id}/auth/templates/preview", "POST /api/v1/projects/{id}/auth/signing-keys/rotate",
		// V4 §9 the Supabase migration helper
		"POST /api/v1/projects/{id}/migrate/supabase",
	},
	// V4 §8.3 API logs, and the auth settings and keys to read
	authz.ServicesLogs: {"GET /api/v1/projects/{id}/services/logs", "GET /api/v1/projects/{id}/auth/config",
		"GET /api/v1/projects/{id}/auth/signing-keys", "GET /api/v1/projects/{id}/auth/hooks"},
	// V4 §4.9 the project's app users
	authz.FilesView: {"GET /api/v1/projects/{id}/files", "GET /api/v1/projects/{id}/files/buckets/{bucket}/objects",
		"GET /api/v1/projects/{id}/files/buckets/{bucket}/object", "POST /api/v1/projects/{id}/files/buckets/{bucket}/sign"},
	authz.FilesManage: {"POST /api/v1/projects/{id}/files/buckets", "PATCH /api/v1/projects/{id}/files/buckets/{bucket}",
		"DELETE /api/v1/projects/{id}/files/buckets/{bucket}", "DELETE /api/v1/projects/{id}/files/buckets/{bucket}/objects",
		"PUT /api/v1/projects/{id}/files/buckets/{bucket}/object"},
	authz.RealtimeView:   {"GET /api/v1/projects/{id}/realtime"},
	authz.RealtimeManage: {"PUT /api/v1/projects/{id}/realtime/tables/{schema}/{table}", "PUT /api/v1/projects/{id}/realtime/persisted-topics"},
	authz.AuthUsers: {"GET /api/v1/projects/{id}/auth/users", "GET /api/v1/projects/{id}/auth/users/{userId}",
		"GET /api/v1/projects/{id}/auth/audit"},
	authz.AuthUsersManage: {"POST /api/v1/projects/{id}/auth/users", "PATCH /api/v1/projects/{id}/auth/users/{userId}",
		"DELETE /api/v1/projects/{id}/auth/users/{userId}", "POST /api/v1/projects/{id}/auth/users/{userId}/signout"},
	// "Get personal DB credentials"
	authz.ProjectCredentials: {"GET /api/v1/projects/{id}/credentials", "POST /api/v1/projects/{id}/credentials"},
	// "SQL console - read" (writes are re-checked in the handler)
	authz.ConsoleRead: {
		"POST /api/v1/projects/{id}/sql", "POST /api/v1/projects/{id}/sql/cancel",
		// V3 §8 query insights read the project's statements and catalog
		"GET /api/v1/projects/{id}/insights/queries", "GET /api/v1/projects/{id}/insights/queries/{query_id}",
		"POST /api/v1/projects/{id}/insights/explain", "GET /api/v1/projects/{id}/insights/slow",
		"GET /api/v1/projects/{id}/insights/indexes", "GET /api/v1/projects/{id}/insights/bloat", "GET /api/v1/projects/{id}/insights/locks",
		"GET /api/v1/projects/{id}/schema", "GET /api/v1/projects/{id}/tables/{schema}/{table}/rows",
		// §4.1 the grid's table details and export; previewing a schema
		// change and rendering it as a migration change nothing
		"GET /api/v1/projects/{id}/tables/{schema}/{table}", "GET /api/v1/projects/{id}/tables/{schema}/{table}/export",
		"GET /api/v1/projects/{id}/tables/{schema}/{table}/count", "GET /api/v1/projects/{id}/tables/{schema}/{table}/definition",
		"POST /api/v1/projects/{id}/schema/preview", "POST /api/v1/projects/{id}/schema/migration",
		"GET /api/v1/projects/{id}/editor-preferences",
		// the SQL Editor's saved queries: your own, and the shared ones
		// (owners edit, admins also delete shared ones: in the handler)
		"GET /api/v1/projects/{id}/queries", "POST /api/v1/projects/{id}/queries",
		"GET /api/v1/projects/{id}/queries/{query_id}", "PATCH /api/v1/projects/{id}/queries/{query_id}",
		"DELETE /api/v1/projects/{id}/queries/{query_id}", "PUT /api/v1/projects/{id}/queries/{query_id}/favorite",
		// V4 §3.6, §3.7: generated types and the security advisor read the schema
		"GET /api/v1/projects/{id}/services/catalog", "GET /api/v1/projects/{id}/services/usage", "GET /api/v1/projects/{id}/services/types", "GET /api/v1/projects/{id}/services/advisor",
	},
	// V4 §8.3 the request explorer runs requests as service, too: a write to the data
	authz.ConsoleWrite: {"POST /api/v1/projects/{id}/services/explore"},
	// "Table editor — rows and schema"
	authz.TableEdit: {"POST /api/v1/projects/{id}/tables/{schema}/{table}/changes", "POST /api/v1/projects/{id}/schema/apply"},
	// "Create backup, restore into new project" (in place re-checked)
	authz.BackupCreate: {"POST /api/v1/projects/{id}/backups", "POST /api/v1/projects/{id}/pitr", "POST /api/v1/backups/{id}/restore"},
	// "Create / reset / delete branches" (deleting a branch is DELETE
	// /projects/{id} with the branch action; detaching keeps one)
	authz.BranchManage: {"POST /api/v1/projects/{id}/branches", "POST /api/v1/projects/{id}/reset", "POST /api/v1/projects/{id}/detach"},
	// "Manage webhooks and scheduled jobs"
	authz.AutomationManage: {
		"GET /api/v1/projects/{id}/webhooks", "POST /api/v1/projects/{id}/webhooks", "GET /api/v1/projects/{id}/webhooks/{webhook_id}",
		"PATCH /api/v1/projects/{id}/webhooks/{webhook_id}", "DELETE /api/v1/projects/{id}/webhooks/{webhook_id}",
		"POST /api/v1/projects/{id}/webhooks/{webhook_id}/test", "POST /api/v1/projects/{id}/webhooks/{webhook_id}/rotate-secret",
		"GET /api/v1/projects/{id}/webhooks/{webhook_id}/deliveries", "POST /api/v1/projects/{id}/webhooks/{webhook_id}/replay",
		"GET /api/v1/projects/{id}/jobs", "POST /api/v1/projects/{id}/jobs", "GET /api/v1/projects/{id}/jobs/{job_id}",
		"PATCH /api/v1/projects/{id}/jobs/{job_id}", "DELETE /api/v1/projects/{id}/jobs/{job_id}",
		"POST /api/v1/projects/{id}/jobs/{job_id}/run", "GET /api/v1/projects/{id}/jobs/{job_id}/runs",
	},
	// "Choose project's storage target, download backup key" (V2 §6)
	authz.BackupStorage: {
		"PUT /api/v1/projects/{id}/storage-target", "POST /api/v1/projects/{id}/backup-key",
		"GET /api/v1/projects/{id}/backup-key/download",
	},
	// "Rotate app password, settings/guardrails, extensions"
	authz.ProjectSettings: {
		"GET /api/v1/projects/{id}/manage-roles", "POST /api/v1/projects/{id}/manage-roles/sync",
		"PATCH /api/v1/projects/{id}/settings", "POST /api/v1/projects/{id}/rotate-password",
		"POST /api/v1/projects/{id}/extensions", "POST /api/v1/projects/{id}/instance",
		// §10.2 switch to opaque credentials (a new app password), §10.4 Reclaim space
		"POST /api/v1/projects/{id}/switch-credentials", "POST /api/v1/projects/{id}/reclaim-space",
	},
	// "Manage project members"
	authz.ProjectMembers: {
		"POST /api/v1/projects/{id}/members", "PATCH /api/v1/projects/{id}/members/{user}", "DELETE /api/v1/projects/{id}/members/{user}",
	},
	// "Promote / demote"
	authz.ProjectPromote: {
		"GET /api/v1/projects/{id}/promote", "POST /api/v1/projects/{id}/promote",
		"POST /api/v1/projects/{id}/demote/preflight", "POST /api/v1/projects/{id}/demote",
		// V3 §2.4: a major upgrade moves the project like a promotion does
		"POST /api/v1/projects/{id}/upgrade/preflight", "POST /api/v1/projects/{id}/upgrade",
		// V3 §2.2 HA: enable/disable, settings, planned switchover, visibility.
		"POST /api/v1/projects/{id}/ha", "PATCH /api/v1/projects/{id}/ha", "PATCH /api/v1/projects/{id}/instance",
		"DELETE /api/v1/projects/{id}/ha", "POST /api/v1/projects/{id}/switchover",
		// V3.1 §3.3 moving its Patroni state to its region's etcd cluster
		"POST /api/v1/projects/{id}/ha/etcd-move",
		// V4 §7 read replicas: create, delete, detach into a project
		"POST /api/v1/projects/{id}/replicas", "DELETE /api/v1/projects/{id}/replicas/{replica_id}",
		"POST /api/v1/projects/{id}/replicas/{replica_id}/detach",
	},
	// "Delete project" (transfer also needs owner of both orgs, checked in the handler)
	authz.ProjectDelete: {"DELETE /api/v1/projects/{id}", "POST /api/v1/projects/{id}/transfer"},
	// Project audit log, for project admins (§2.7)
	authz.ProjectAudit: {"GET /api/v1/projects/{id}/audit"},
	// Seeing the organisation and its lists
	authz.OrgView: {
		// V3 §7.1 "Tickets can also be opened from the dashboard": members see their own, admins the org's
		"GET /api/v1/orgs/{org}/support/tickets", "POST /api/v1/orgs/{org}/support/tickets",
		"GET /api/v1/orgs/{org}/support/tickets/{ticket_id}", "POST /api/v1/orgs/{org}/support/tickets/{ticket_id}/messages",
		"GET /api/v1/orgs/{org}/support/phones",
		// V3 §7.3: members read the documents in effect
		"GET /api/v1/orgs/{org}/legal",
		"GET /api/v1/orgs/{org}", "GET /api/v1/orgs/{org}/members", "GET /api/v1/orgs/{org}/incidents", "POST /api/v1/orgs/{org}/leave",
		"GET /api/v1/projects", "GET /api/v1/operations", "GET /api/v1/backups", "GET /api/v1/backups/overview",
		// §13 "projects list ... with quota usage bars": every member sees the limits
		"GET /api/v1/orgs/{org}/quotas",
		// V3 §3.10 "Cost estimate before every billable action"
		"POST /api/v1/orgs/{org}/billing/estimate",
	},
	// "Create projects, import"
	authz.OrgCreateProject: {"POST /api/v1/projects", "POST /api/v1/imports"},
	// "Manage org members and invitations", "org settings"
	authz.OrgManage: {
		// V3 §7.1 WhatsApp "linked to the org by the registered phone number"
		"POST /api/v1/orgs/{org}/support/phones", "DELETE /api/v1/orgs/{org}/support/phones/{phone}",
		"PATCH /api/v1/orgs/{org}", "POST /api/v1/orgs/{org}/members", "PATCH /api/v1/orgs/{org}/members/{user}",
		"DELETE /api/v1/orgs/{org}/members/{user}", "GET /api/v1/orgs/{org}/invitations", "DELETE /api/v1/orgs/{org}/invitations/{invitation_id}",
		// §2.3 "See and revoke any token scoped to the org"
		"GET /api/v1/orgs/{org}/tokens", "DELETE /api/v1/orgs/{org}/tokens/{token_id}",
		// V2 §6 "Org targets (managed by org owners and admins)"
		"GET /api/v1/orgs/{org}/storage-targets", "POST /api/v1/orgs/{org}/storage-targets",
		"GET /api/v1/orgs/{org}/storage-targets/{target_id}", "PATCH /api/v1/orgs/{org}/storage-targets/{target_id}",
		"DELETE /api/v1/orgs/{org}/storage-targets/{target_id}", "POST /api/v1/storage-targets/test",
	},
	// "View org usage and quotas, org audit log"
	authz.OrgAudit: {"GET /api/v1/orgs/{org}/audit", "GET /api/v1/orgs/{org}/usage", "GET /api/v1/orgs/{org}/dedicated-requests"},
	// "Add/remove owners, delete org, transfer projects out"
	authz.OrgOwnerOnly: {
		"POST /api/v1/orgs/{org}/transfer-ownership", "DELETE /api/v1/orgs/{org}", "POST /api/v1/orgs/{org}/cancel-deletion",
		// §2.4 "any org owner can end the session early"
		"POST /api/v1/orgs/{org}/break-glass/{session_id}/end",
		// V3 §7.3 legal documents "accepted" by the organisation
		"POST /api/v1/orgs/{org}/legal/{document_id}/accept",
	},
	// V3 §3.2 "Only org owners and members with a new billing org role can see or change billing"
	authz.OrgBillingManage: {
		"GET /api/v1/orgs/{org}/billing", "PATCH /api/v1/orgs/{org}/billing", "POST /api/v1/orgs/{org}/billing/plan",
		"GET /api/v1/orgs/{org}/billing/contacts", "POST /api/v1/orgs/{org}/billing/contacts",
		"DELETE /api/v1/orgs/{org}/billing/contacts/{email}", "PATCH /api/v1/orgs/{org}/billing/contacts/{email}", "GET /api/v1/orgs/{org}/billing/invoices",
		"GET /api/v1/orgs/{org}/billing/invoices/{invoice_id}", "GET /api/v1/orgs/{org}/billing/invoices/{invoice_id}/pdf",
		"GET /api/v1/orgs/{org}/billing/forecast",
		// V3 §3.4: paying, methods, receipts, WHT credit notes
		"GET /api/v1/orgs/{org}/billing/wallet", "POST /api/v1/orgs/{org}/billing/wallet", "POST /api/v1/orgs/{org}/billing/debit-consent", "DELETE /api/v1/orgs/{org}/billing/debit-consent", "POST /api/v1/orgs/{org}/billing/invoices/{invoice_id}/pay-from-wallet",
		"POST /api/v1/orgs/{org}/billing/virtual-account", "GET /api/v1/orgs/{org}/billing/payments", "GET /api/v1/orgs/{org}/billing/payments/{payment_id}/receipt", "POST /api/v1/orgs/{org}/billing/invoices/{invoice_id}/wht-certificate",
	},
	// §10.10 "Org owners can export any project as a pg_dump file"
	authz.ProjectExport: {"GET /api/v1/backups/{id}/download"},
	// V3 §6.3 the data residency setting: owners only
	authz.ProjectResidency: {"PUT /api/v1/projects/{id}/residency"},
	// The owner's CREATEROLE on a dedicated project: owners only
	authz.ProjectManageRoles: {"PUT /api/v1/projects/{id}/manage-roles"},
	// The signed-in user's own account
	authz.Self: {
		"POST /api/v1/auth/reauth", "POST /api/v1/auth/logout", "GET /api/v1/me", "PATCH /api/v1/me", "POST /api/v1/me/password",
		"GET /api/v1/me/sessions", "DELETE /api/v1/me/sessions/{session_id}", "GET /api/v1/me/recovery-codes",
		"POST /api/v1/me/recovery-codes", "GET /api/v1/me/totp", "POST /api/v1/me/totp/setup", "POST /api/v1/me/totp/enable",
		"POST /api/v1/me/totp/disable", "POST /api/v1/me/terms/accept", "GET /api/v1/me/invitations",
		"POST /api/v1/me/invitations/{invitation_id}/accept", "GET /api/v1/orgs", "POST /api/v1/orgs",
		"GET /api/v1/settings/general", "GET /api/v1/profiles", "POST /api/v1/imports/preflight",
		// V3 §6.1 "Projects choose a region at creation"
		"GET /api/v1/regions",
		// §7.2 "Users manage their own tokens", §7.1 device-login approval
		"GET /api/v1/tokens", "POST /api/v1/tokens", "DELETE /api/v1/tokens/{token_id}",
		"GET /api/v1/auth/device/requests/{user_code}", "POST /api/v1/auth/device/approve",
		// OAuth for third-party apps: consenting is the user's own act
		"GET /api/v1/oauth/authorize", "POST /api/v1/oauth/authorize",
	},
	// §2.4: the platform admin's
	// V3 §7.1 "Support staff role ... can see the support console and org metadata"
	authz.SupportConsole: {
		"GET /api/v1/admin/support/tickets", "GET /api/v1/admin/support/tickets/{ticket_id}",
		"PATCH /api/v1/admin/support/tickets/{ticket_id}", "POST /api/v1/admin/support/tickets/{ticket_id}/messages",
		"GET /api/v1/admin/support/staff",
	},
	authz.PlatformManage: {
		// V3 §7.2 "Revenue and cost dashboard ... For the platform admin"
		"GET /api/v1/admin/revenue",
		// V3 §7.3 versioned legal documents and order forms
		"GET /api/v1/admin/legal", "POST /api/v1/admin/legal", "GET /api/v1/admin/legal/{document_id}", "POST /api/v1/admin/orgs/{org}/order-form",
		"PUT /api/v1/settings/db-host", "POST /api/v1/settings/db-host/check", "POST /api/v1/dev/operations",
		"POST /api/v1/restore-tests", "GET /api/v1/settings/storage", "PUT /api/v1/settings/storage",
		"POST /api/v1/settings/storage/test", "GET /api/v1/settings/backup-key", "POST /api/v1/settings/backup-key",
		"POST /api/v1/settings/backup-key/export", "POST /api/v1/settings/backup-key/confirm", "GET /api/v1/nodes",
		"POST /api/v1/nodes", "GET /api/v1/nodes/{id}", "PATCH /api/v1/nodes/{id}", "DELETE /api/v1/nodes/{id}",
		// V3 §5 capacity automation, §5.4 and §7.2 costs and margins
		// V3 §6.1 regions
		"GET /api/v1/admin/pg-versions", "PATCH /api/v1/admin/pg-versions/{major}", "GET /api/v1/admin/regions", "PUT /api/v1/admin/regions/{region_id}", "GET /api/v1/admin/regions/{region_id}/readiness",
		"GET /api/v1/admin/capacity", "PUT /api/v1/admin/capacity/settings", "POST /api/v1/admin/capacity/evaluate", "POST /api/v1/admin/capacity/proposals/{proposal_id}/approve", "POST /api/v1/admin/capacity/proposals/{proposal_id}/reject", "POST /api/v1/admin/capacity/rebalance", "POST /api/v1/admin/capacity/batches/{batch_id}", "GET /api/v1/admin/cloud/catalog", "POST /api/v1/nodes/{id}/drain", "DELETE /api/v1/nodes/{id}/drain", "PUT /api/v1/nodes/{id}/cost", "GET /api/v1/admin/costs", "POST /api/v1/admin/costs/attribute", "GET /api/v1/admin/costs/settings", "PUT /api/v1/admin/costs/settings", "GET /api/v1/admin/fx-rates", "POST /api/v1/admin/fx-rates",
		"POST /api/v1/nodes/{id}/shared-cluster", "POST /api/v1/nodes/{id}/registration-token", "GET /api/v1/nodes/{id}/metrics",
		"GET /api/v1/security/isolation-checks", "POST /api/v1/security/isolation-checks", "GET /api/v1/alerts",
		"GET /api/v1/settings/alerts", "PUT /api/v1/settings/alerts", "POST /api/v1/settings/alerts/test",
		"GET /api/v1/admin/audit", "GET /api/v1/admin/users", "PATCH /api/v1/admin/users/{user}",
		"POST /api/v1/admin/users/{user}/reset-2fa", "GET /api/v1/admin/invitations", "POST /api/v1/admin/invitations",
		"DELETE /api/v1/admin/invitations/{invitation_id}", "GET /api/v1/admin/settings/signup", "PUT /api/v1/admin/settings/signup",
		"GET /api/v1/admin/settings/mail", "PUT /api/v1/admin/settings/mail", "POST /api/v1/admin/settings/terms",
		// §2.4 "assign quota plans and dedicated allowances; approve dedicated requests", "suspend and reinstate", break-glass
		"GET /api/v1/admin/orgs", "GET /api/v1/admin/orgs/{org}", "PATCH /api/v1/admin/orgs/{org}",
		"POST /api/v1/admin/orgs/{org}/suspend", "POST /api/v1/admin/orgs/{org}/reinstate", "POST /api/v1/admin/orgs/{org}/cluster",
		"GET /api/v1/admin/orgs/{org}/outbound", "PUT /api/v1/admin/orgs/{org}/outbound",
		"POST /api/v1/admin/orgs/{org}/break-glass", "GET /api/v1/admin/plans", "POST /api/v1/admin/plans",
		"PATCH /api/v1/admin/plans/{plan_id}", "GET /api/v1/admin/dedicated-requests",
		"POST /api/v1/admin/dedicated-requests/{request_id}/approve", "POST /api/v1/admin/dedicated-requests/{request_id}/reject",
		"GET /api/v1/admin/usage", "GET /api/v1/admin/shared-clusters",
		// V3 §2.2 "a 3-member etcd cluster spread across the control node and two other nodes"
		"GET /api/v1/admin/etcd", "POST /api/v1/admin/etcd",
		// V3.1 §2.4 groups that share a failure domain
		"GET /api/v1/admin/failure-domains",
		// V3.1 §4 announced maintenance
		"GET /api/v1/admin/maintenance/announcements", "POST /api/v1/admin/maintenance/announcements",
		"GET /api/v1/admin/regions/{region_id}/overview", "POST /api/v1/admin/regions/{region_id}/etcd-move-all",
		"DELETE /api/v1/admin/maintenance/announcements/{incident_id}", "POST /api/v1/admin/maintenance/announcements/preview",
		"POST /api/v1/admin/maintenance/announcements/{incident_id}/confirm", "POST /api/v1/admin/maintenance/announcements/{incident_id}/discard",
		// V3.1 §3.2 replacing an etcd member
		"POST /api/v1/admin/etcd/members/{node_id}/replace",
		// V3 §2.4 "Minor upgrades ... automated in a weekly maintenance window"
		"GET /api/v1/admin/maintenance", "PUT /api/v1/admin/maintenance/window",
		// V3 §3.6, §3.9: billing settings, price books, an org's billing terms
		"GET /api/v1/admin/billing/settings", "PUT /api/v1/admin/billing/settings",
		"GET /api/v1/admin/price-books", "POST /api/v1/admin/price-books", "GET /api/v1/admin/price-books/{version}",
		"PUT /api/v1/admin/price-books/{version}", "DELETE /api/v1/admin/price-books/{version}",
		"POST /api/v1/admin/price-books/{version}/publish", "GET /api/v1/admin/orgs/{org}/billing", "PATCH /api/v1/admin/orgs/{org}/billing",
		"POST /api/v1/admin/price-books/{version}/preview", "GET /api/v1/admin/invoices", "POST /api/v1/admin/invoices/draft",
		"GET /api/v1/admin/invoices/{invoice_id}", "GET /api/v1/admin/invoices/{invoice_id}/pdf",
		"POST /api/v1/admin/invoices/{invoice_id}/hold", "POST /api/v1/admin/invoices/{invoice_id}/issue",
		"POST /api/v1/admin/invoices/{invoice_id}/credit-notes", "GET /api/v1/admin/ledger/check",
		"GET /api/v1/admin/payments", "POST /api/v1/admin/payments", "POST /api/v1/admin/billing/documents", "POST /api/v1/admin/payments/{payment_id}/refund", "GET /api/v1/admin/payment-events", "POST /api/v1/admin/payment-events/{event_id}/attribute", "GET /api/v1/admin/wht", "POST /api/v1/admin/invoices/{invoice_id}/wht-certificate", "GET /api/v1/admin/reconciliation", "POST /api/v1/admin/reconciliation", "PUT /api/v1/admin/orgs/{org}/billing/grace",
		"POST /api/v1/admin/instances/{instance_id}/minor-upgrade",
		// §7.2 "The platform admin can set a platform-wide maximum"
		"GET /api/v1/admin/settings/tokens", "PUT /api/v1/admin/settings/tokens",
		// OAuth for third-party apps: the platform admin registers clients
		"GET /api/v1/admin/oauth/clients", "POST /api/v1/admin/oauth/clients", "DELETE /api/v1/admin/oauth/clients/{client_id}",
		// Integrator roles on a dedicated instance: the platform's superuser creates them
		"POST /api/v1/admin/projects/{project_id}/roles",
		// V2 §6 "Platform targets (managed by the platform admin)"
		"GET /api/v1/admin/storage-targets", "POST /api/v1/admin/storage-targets",
		"GET /api/v1/admin/storage-targets/{target_id}", "PATCH /api/v1/admin/storage-targets/{target_id}",
		"DELETE /api/v1/admin/storage-targets/{target_id}",
		// V3 §2.6 "manually by the platform admin. Updates are posted from
		// the admin console"; §2.1 the pooler hosts are platform infrastructure.
		"GET /api/v1/incidents", "POST /api/v1/incidents", "GET /api/v1/incidents/{id}",
		"PATCH /api/v1/incidents/{id}", "POST /api/v1/incidents/{id}/updates", "GET /api/v1/pooler-hosts",
		// V3 §5.3: moving projects between nodes is the platform's call
		"POST /api/v1/admin/projects/{project_id}/move",
	},
}

// TestRoutesMatchTheSpecMatrix checks every route's declared action
// against specMatrix (and that every non-public route is in it once).
func TestRoutesMatchTheSpecMatrix(t *testing.T) {
	want := map[string]authz.Action{}
	for action, routes := range specMatrix {
		for _, r := range routes {
			if prev, dup := want[r]; dup {
				t.Errorf("%s is listed under both %s and %s", r, prev, action)
			}
			want[r] = action
		}
	}
	for route, rl := range routeRules {
		w, listed := want[route]
		switch {
		case rl.scope == scopePublic:
			if listed {
				t.Errorf("%s is public but listed under %s", route, w)
			}
		case !listed:
			t.Errorf("%s (%s) is not in the spec matrix", route, rl.action)
		case w != rl.action:
			t.Errorf("%s declares %s; the spec says %s", route, rl.action, w)
		}
	}
	for r := range want {
		if _, ok := routeRules[r]; !ok {
			t.Errorf("spec matrix lists %s, which has no rule", r)
		}
	}
}
