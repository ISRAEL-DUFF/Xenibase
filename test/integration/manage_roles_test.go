package integration

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/test/testenv"
)

// TestDedicatedManageRoles is Taskiem's dedicated-database request: with
// role management on, a dedicated project's owner runs migrations that
// create group roles and logins, the logins connect through the poolers,
// the owner can't reach Xenibase's roles or hand out what it lacks, and a
// move takes the roles along.
func TestDedicatedManageRoles(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	e.ConfigureBackups()
	e.SetNodeRole("test", "both")
	ctx := context.Background()

	tier := gen.ProjectTierDedicated
	profile, vol := "small", 5
	var c gen.ProjectCredentials
	if code := e.Do("POST", "/api/v1/projects", gen.CreateProjectRequest{Name: "Taskiem prod", Tier: &tier, Profile: &profile, VolumeGb: &vol}, &c); code != http.StatusAccepted {
		t.Fatalf("create dedicated: %d", code)
	}
	if op := e.WaitOperation(c.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("create: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	pid, db := c.Project.Id.String(), c.Project.DbName
	owner := e.MustConnect(c.Connection.SessionUrl)
	defer owner.Close(ctx)

	// ---- Off: the owner can't create roles -------------------------------------------------
	if _, err := owner.Exec(ctx, `CREATE ROLE taskiem_app NOLOGIN`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("CREATE ROLE with role management off: %v", err)
	}
	var mr gen.ManageRoles
	if code := e.Do("PUT", "/api/v1/projects/"+pid+"/manage-roles", gen.ManageRolesRequest{Enabled: true}, &mr); code != http.StatusOK || !mr.Enabled {
		t.Fatalf("turn on: %d %+v", code, mr)
	}

	// ---- On: Taskiem's migrations and installer, unchanged ----------------------------------
	for _, stmt := range []string{
		`CREATE ROLE taskiem_app NOLOGIN NOBYPASSRLS`,
		`CREATE ROLE taskiem_dispatch NOLOGIN NOBYPASSRLS`,
		`CREATE ROLE taskiem_public NOLOGIN NOBYPASSRLS`,
		`CREATE TABLE runs (id bigserial PRIMARY KEY, tenant text NOT NULL, step text NOT NULL)`,
		`ALTER TABLE runs ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY tenant_runs ON runs TO taskiem_app USING (tenant = current_setting('taskiem.tenant', true))`,
		`GRANT SELECT, INSERT ON runs TO taskiem_app`,
		`GRANT USAGE ON SEQUENCE runs_id_seq TO taskiem_app`,
		`INSERT INTO runs (tenant, step) VALUES ('acme', 'one'), ('globex', 'two')`,
		`CREATE ROLE ` + db + `_app LOGIN PASSWORD 'app-password-1' IN ROLE taskiem_app`,
		`CREATE ROLE ` + db + `_public LOGIN NOINHERIT PASSWORD 'public-password-1'`,
		`GRANT taskiem_public TO ` + db + `_public`,
		// Not synced: not named after the database.
		`CREATE ROLE stray_login LOGIN PASSWORD 'stray-password-1'`,
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// ---- What the owner still can't do ------------------------------------------------------
	for _, stmt := range []string{
		`CREATE ROLE sneaky SUPERUSER`,
		`CREATE ROLE sneaky2 BYPASSRLS`,
		`CREATE ROLE sneaky3 REPLICATION`,
		`ALTER ROLE xenibase PASSWORD 'x'`,
		`DROP ROLE ` + db + `_ro`,
		`GRANT pg_read_server_files TO ` + db + `_app`,
	} {
		if _, err := owner.Exec(ctx, stmt); err == nil {
			t.Fatalf("the owner could run %s", stmt)
		}
	}

	// ---- Sync: the logins connect through the poolers ---------------------------------------
	if code := e.Do("POST", "/api/v1/projects/"+pid+"/manage-roles/sync", nil, &mr); code != http.StatusOK {
		t.Fatalf("sync: %d", code)
	}
	names := []string{}
	for _, l := range mr.Logins {
		names = append(names, l.Name)
	}
	if !slices.Equal(names, []string{db + "_app", db + "_public"}) {
		t.Fatalf("synced logins: %v", names)
	}
	if mr.Skipped == nil || !slices.ContainsFunc(*mr.Skipped, func(s struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}) bool {
		return s.Name == "stray_login"
	}) {
		t.Fatalf("stray_login should be listed as skipped: %+v", mr.Skipped)
	}
	asLogin := func(raw, user, pass string) string {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(user, pass)
		return u.String()
	}
	appURL := asLogin(c.Connection.SessionUrl, db+"_app", "app-password-1")
	checkApp := func(when string) {
		t.Helper()
		app, err := pgx.Connect(ctx, appURL)
		if err != nil {
			t.Fatalf("%s: connect as %s_app through the session pooler: %v", when, db, err)
		}
		defer app.Close(ctx)
		var n int
		if err := app.QueryRow(ctx, `SELECT count(*) FROM (SELECT set_config('taskiem.tenant', 'acme', false)) s, runs`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: row-level security as the app login: %d %v", when, n, err)
		}
		// LISTEN/NOTIFY on a session connection (the task queue).
		if _, err := app.Exec(ctx, `LISTEN taskiem_queue`); err != nil {
			t.Fatalf("%s: LISTEN: %v", when, err)
		}
	}
	checkApp("after the sync")
	if _, err := pgx.Connect(ctx, asLogin(c.Connection.SessionUrl, "stray_login", "stray-password-1")); err == nil {
		t.Fatal("a login not named after the database connected through the pooler")
	}

	// ---- The connection limit is capped at the project's -----------------------------------
	var limit int
	if err := owner.QueryRow(ctx, `SELECT rolconnlimit FROM pg_roles WHERE rolname = $1`, db+"_app").Scan(&limit); err != nil || limit < 1 {
		t.Fatalf("%s_app connection limit: %d %v", db, limit, err)
	}

	// ---- A move takes the roles along -------------------------------------------------------
	var created gen.NodeCreated
	if code := e.Do("POST", "/api/v1/nodes", gen.CreateNodeRequest{Name: "node-b", PrivateAddr: "agent-test-2", Role: gen.CreateNodeRequestRoleDedicated}, &created); code != http.StatusCreated {
		t.Fatalf("create node: %d", code)
	}
	e.StartSecondAgent("node-b", created.Token)
	_ = owner.Close(ctx)
	op, mv := moveAndWait(t, e, pid, created.Node.Id.String())
	if !strings.Contains(testenv.FormatLog(op), "the owner's") {
		t.Fatalf("the move log doesn't mention the owner's roles:\n%s", testenv.FormatLog(op))
	}
	t.Logf("moved by %s", mv.Mode)
	checkApp("after the move")
	moved := e.MustConnect(c.Connection.SessionUrl)
	defer moved.Close(ctx)
	var canCreate bool
	if err := moved.QueryRow(ctx, `SELECT rolcreaterole FROM pg_roles WHERE rolname = current_user`).Scan(&canCreate); err != nil || !canCreate {
		t.Fatalf("the owner's CREATEROLE after the move: %v %v", canCreate, err)
	}
	if _, err := moved.Exec(ctx, `DROP ROLE stray_login`); err != nil {
		t.Fatalf("the owner manages its roles after the move: %v", err)
	}

	// ---- Off: no more CREATEROLE; existing logins keep working -------------------------------
	if code := e.Do("PUT", "/api/v1/projects/"+pid+"/manage-roles", gen.ManageRolesRequest{Enabled: false}, &mr); code != http.StatusOK || mr.Enabled {
		t.Fatalf("turn off: %d %+v", code, mr)
	}
	if _, err := moved.Exec(ctx, `CREATE ROLE taskiem_later NOLOGIN`); err == nil {
		t.Fatal("CREATE ROLE after turning role management off")
	}
	checkApp("after turning it off")

	// ---- Shared projects can't have it -------------------------------------------------------
	shared := e.CreateProject("Shared neighbour")
	var er gen.Error
	if code := e.Do("PUT", "/api/v1/projects/"+shared.Project.Id.String()+"/manage-roles", gen.ManageRolesRequest{Enabled: true}, &er); code != http.StatusConflict {
		t.Fatalf("a shared project: %d %+v", code, er)
	}
}
