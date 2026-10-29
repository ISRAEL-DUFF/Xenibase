// Package isolation is the tenant-escape suite (spec §7.1): it creates two
// throwaway shared-tier projects, A and B, and asserts that A cannot
// connect to B's database, see B's objects, read B's data through any
// predefined role, create objects in B, interfere with B's sessions, gain
// privileges, or read server files. V2 adds metadata leaks (descriptive
// names in the catalogs), quota bypass attempts, and suspended-org checks.
// It runs in CI, and nightly against live nodes.
package isolation

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/isocheck"
	"github.com/israel-duff/xenibase/test/testenv"
)

type tenant struct {
	creds gen.ProjectCredentials
	conn  *pgx.Conn // A's own database, through the session pooler
}

func (tn tenant) db() string   { return tn.creds.Project.DbName }
func (tn tenant) role() string { return tn.creds.Project.OwnerRole }

func setup(t *testing.T) (*testenv.Env, tenant, tenant) {
	t.Helper()
	e := testenv.Start(t, testenv.Options{})
	a := e.CreateProject("isolation a")
	b := e.CreateProject("isolation b")
	ta := tenant{a, e.MustConnect(a.Connection.SessionUrl)}
	tb := tenant{b, e.MustConnect(b.Connection.SessionUrl)}

	// B keeps a secret A must never reach.
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE secret_b (v text)`,
		`INSERT INTO secret_b VALUES ('b-only')`,
		`CREATE SCHEMA private_b`,
	} {
		if _, err := tb.conn.Exec(ctx, q); err != nil {
			t.Fatalf("seed B: %s: %v", q, err)
		}
	}
	return e, ta, tb
}

// mustFail runs q as A and requires an error mentioning one of wants.
func mustFail(t *testing.T, conn *pgx.Conn, q string, wants ...string) {
	t.Helper()
	_, err := conn.Exec(context.Background(), q)
	if err == nil {
		t.Errorf("A succeeded at: %s", q)
		return
	}
	msg := strings.ToLower(err.Error())
	for _, w := range wants {
		if strings.Contains(msg, w) {
			return
		}
	}
	t.Errorf("%s: unexpected error %q (want one of %q)", q, err, wants)
}

func mustNotConnect(t *testing.T, e *testenv.Env, what, url string) {
	t.Helper()
	conn, err := e.Connect(url)
	if err == nil {
		_ = conn.Close(context.Background())
		t.Errorf("%s: A connected (%s)", what, testenv.RedactURL(url))
	}
}

func TestTenantIsolation(t *testing.T) {
	e, a, b := setup(t)
	ctx := context.Background()

	t.Run("cannot connect to B's database", func(t *testing.T) {
		pooled := testenv.WithDatabase(t, a.creds.Connection.PooledUrl, b.db())
		session := testenv.WithDatabase(t, a.creds.Connection.SessionUrl, b.db())
		mustNotConnect(t, e, "through the transaction pooler", pooled)
		mustNotConnect(t, e, "through the session pooler", session)
		mustNotConnect(t, e, "directly on the backend", e.DirectURL(a.creds.Connection.SessionUrl, b.db()))
	})

	t.Run("cannot connect to maintenance databases", func(t *testing.T) {
		for _, db := range []string{"postgres", "template1", "template0"} {
			mustNotConnect(t, e, db+" directly", e.DirectURL(a.creds.Connection.SessionUrl, db))
			mustNotConnect(t, e, db+" through the pooler", testenv.WithDatabase(t, a.creds.Connection.SessionUrl, db))
		}
	})

	t.Run("B's credentials are not A's", func(t *testing.T) {
		// A's password does not authenticate as B's role.
		url := strings.Replace(a.creds.Connection.SessionUrl, a.role()+":", b.role()+":", 1)
		url = testenv.WithDatabase(t, url, b.db())
		mustNotConnect(t, e, "B's role with A's password", url)
	})

	t.Run("cannot see B's objects", func(t *testing.T) {
		var n int
		if err := a.conn.QueryRow(ctx,
			`SELECT count(*) FROM pg_class WHERE relname = 'secret_b'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("A sees B's table: %d %v", n, err)
		}
		if err := a.conn.QueryRow(ctx,
			`SELECT count(*) FROM pg_namespace WHERE nspname = 'private_b'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("A sees B's schema: %d %v", n, err)
		}
		mustFail(t, a.conn, `SELECT * FROM secret_b`, "does not exist")
		mustFail(t, a.conn, `SELECT * FROM `+pgx.Identifier{b.db(), "public", "secret_b"}.Sanitize(), "cross-database references are not implemented")
	})

	t.Run("role has no dangerous attributes", func(t *testing.T) {
		var super, createdb, createrole, repl, bypass bool
		var connLimit int
		if err := a.conn.QueryRow(ctx, `SELECT rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls, rolconnlimit
			FROM pg_roles WHERE rolname = current_user`).Scan(&super, &createdb, &createrole, &repl, &bypass, &connLimit); err != nil {
			t.Fatal(err)
		}
		if super || createdb || createrole || repl || bypass {
			t.Errorf("A has attributes: super=%v createdb=%v createrole=%v replication=%v bypassrls=%v",
				super, createdb, createrole, repl, bypass)
		}
		if connLimit != 20 {
			t.Errorf("connection limit %d, want 20", connLimit)
		}
		mustFail(t, a.conn, `ALTER ROLE CURRENT_USER SUPERUSER`, "permission denied")
		mustFail(t, a.conn, `ALTER ROLE CURRENT_USER CREATEDB`, "permission denied")
		mustFail(t, a.conn, `ALTER ROLE CURRENT_USER BYPASSRLS`, "permission denied")
		mustFail(t, a.conn, `ALTER ROLE CURRENT_USER CONNECTION LIMIT -1`, "permission denied")
		mustFail(t, a.conn, `CREATE DATABASE escape_attempt`, "permission denied")
		mustFail(t, a.conn, `CREATE ROLE escape_attempt`, "permission denied")
	})

	t.Run("not a member of predefined roles", func(t *testing.T) {
		rows, err := a.conn.Query(ctx, `
			SELECT r.rolname FROM pg_roles r
			WHERE r.rolname LIKE 'pg\_%' AND r.rolname <> 'pg_database_owner'
			  AND pg_has_role(current_user, r.oid, 'MEMBER')`)
		if err != nil {
			t.Fatal(err)
		}
		member, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		if len(member) > 0 {
			t.Errorf("A is a member of %v", member)
		}
		for _, r := range []string{"pg_read_all_data", "pg_write_all_data", "pg_read_server_files",
			"pg_write_server_files", "pg_execute_server_program", "pg_signal_backend"} {
			mustFail(t, a.conn, "GRANT "+r+" TO CURRENT_USER", "permission denied", "must have admin option")
		}
	})

	t.Run("cannot act on B's role or database", func(t *testing.T) {
		mustFail(t, a.conn, "SET ROLE "+pgx.Identifier{b.role()}.Sanitize(), "permission denied")
		mustFail(t, a.conn, "SET SESSION AUTHORIZATION "+pgx.Identifier{b.role()}.Sanitize(), "permission denied")
		mustFail(t, a.conn, "ALTER ROLE "+pgx.Identifier{b.role()}.Sanitize()+" PASSWORD 'x'", "permission denied")
		mustFail(t, a.conn, "DROP ROLE "+pgx.Identifier{b.role()}.Sanitize(), "permission denied")
		mustFail(t, a.conn, "DROP DATABASE "+pgx.Identifier{b.db()}.Sanitize(), "must be owner", "permission denied")
		mustFail(t, a.conn, "ALTER DATABASE "+pgx.Identifier{b.db()}.Sanitize()+" CONNECTION LIMIT 0", "must be owner", "permission denied")
		mustFail(t, a.conn, "GRANT CONNECT ON DATABASE "+pgx.Identifier{b.db()}.Sanitize()+" TO CURRENT_USER", "permission denied", "no privileges")
	})

	t.Run("cannot see or signal B's sessions", func(t *testing.T) {
		var bpid int
		if err := b.conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&bpid); err != nil {
			t.Fatal(err)
		}
		// V2 §10.2: on a shared cluster tenants cannot read
		// pg_stat_activity at all, not even other sessions' names.
		mustFail(t, a.conn, `SELECT count(*) FROM pg_stat_activity`, "permission denied")
		mustFail(t, a.conn, `SELECT count(*) FROM pg_stat_get_activity(NULL)`, "permission denied")
		// Nor B's database's activity, or B's backends' database and role.
		mustFail(t, a.conn, `SELECT count(*) FROM pg_stat_database`, "permission denied")
		mustFail(t, a.conn, `SELECT count(*) FROM pg_stat_database_conflicts`, "permission denied")
		mustFail(t, a.conn, `SELECT pg_stat_get_db_xact_commit(oid) FROM pg_database WHERE datname = '`+b.db()+`'`, "permission denied")
		mustFail(t, a.conn, `SELECT pg_stat_get_db_numbackends(oid) FROM pg_database WHERE datname = '`+b.db()+`'`, "permission denied")
		mustFail(t, a.conn, `SELECT pg_stat_get_backend_dbid(s) FROM pg_stat_get_backend_idset() s`, "permission denied")
		mustFail(t, a.conn, `SELECT pg_stat_get_backend_userid(s) FROM pg_stat_get_backend_idset() s`, "permission denied")
		mustFail(t, a.conn, "SELECT pg_terminate_backend("+itoa(bpid)+")", "permission denied", "must be a member")
		mustFail(t, a.conn, "SELECT pg_cancel_backend("+itoa(bpid)+")", "permission denied", "must be a member")
		if _, err := b.conn.Exec(ctx, "SELECT 1"); err != nil {
			t.Errorf("B's session was disturbed: %v", err)
		}
	})

	t.Run("public has no access to project databases", func(t *testing.T) {
		for _, db := range []string{a.db(), b.db()} {
			var connect, temp bool
			if err := a.conn.QueryRow(ctx, `SELECT has_database_privilege('public', $1, 'CONNECT'),
				has_database_privilege('public', $1, 'TEMPORARY')`, db).Scan(&connect, &temp); err != nil {
				t.Fatal(err)
			}
			if connect || temp {
				t.Errorf("PUBLIC has CONNECT=%v TEMPORARY=%v on %s", connect, temp, db)
			}
		}
		var create bool
		if err := a.conn.QueryRow(ctx, `SELECT has_schema_privilege('public', 'public', 'CREATE')`).Scan(&create); err != nil || create {
			t.Errorf("PUBLIC can CREATE in schema public: %v %v", create, err)
		}
		var owner string
		if err := a.conn.QueryRow(ctx, `SELECT nspowner::regrole::text FROM pg_namespace WHERE nspname = 'public'`).Scan(&owner); err != nil || owner != a.role() {
			t.Errorf("schema public owned by %q, want %s (%v)", owner, a.role(), err)
		}
	})

	t.Run("cannot read server files or run programs", func(t *testing.T) {
		if _, err := a.conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS sink (line text)`); err != nil {
			t.Fatal(err)
		}
		mustFail(t, a.conn, `SELECT pg_read_file('/etc/passwd')`, "permission denied")
		mustFail(t, a.conn, `SELECT pg_read_binary_file('postgresql.conf')`, "permission denied")
		mustFail(t, a.conn, `SELECT pg_ls_dir('.')`, "permission denied")
		mustFail(t, a.conn, `SELECT pg_stat_file('postgresql.conf')`, "permission denied")
		mustFail(t, a.conn, `COPY sink FROM '/etc/passwd'`, "permission denied")
		mustFail(t, a.conn, `COPY sink TO '/tmp/xenibase_escape'`, "permission denied")
		mustFail(t, a.conn, `COPY sink FROM PROGRAM 'id'`, "permission denied")
		mustFail(t, a.conn, `SELECT lo_import('/etc/passwd')`, "permission denied")
		mustFail(t, a.conn, `SELECT lo_export(0, '/tmp/xenibase_escape')`, "permission denied")
	})

	t.Run("no escape-prone extensions or untrusted languages", func(t *testing.T) {
		for _, ext := range []string{"dblink", "postgres_fdw", "file_fdw", "adminpack", "plpython3u", "plperlu", "pageinspect", "pg_buffercache"} {
			mustFail(t, a.conn, "CREATE EXTENSION "+ext, "permission denied", "not available", "could not open extension control file", "must be superuser", "is not supported")
		}
		mustFail(t, a.conn, `CREATE LANGUAGE plpython3u`, "permission denied", "must be superuser", "not available", "could not open", "does not exist")
		mustFail(t, a.conn, `CREATE FUNCTION evil() RETURNS int AS 'libc.so.6', 'getpid' LANGUAGE c`, "permission denied")
		var langs []string
		rows, err := a.conn.Query(ctx, `SELECT lanname FROM pg_language WHERE NOT lanpltrusted AND lanname NOT IN ('internal', 'c')`)
		if err != nil {
			t.Fatal(err)
		}
		langs, err = pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil || len(langs) > 0 {
			t.Errorf("untrusted languages installed: %v %v", langs, err)
		}
	})

	t.Run("cannot change superuser-only settings", func(t *testing.T) {
		mustFail(t, a.conn, `SET log_statement = 'none'`, "permission denied")
		mustFail(t, a.conn, `ALTER SYSTEM SET log_statement = 'all'`, "permission denied", "must be superuser")
		mustFail(t, a.conn, "ALTER DATABASE "+pgx.Identifier{a.db()}.Sanitize()+" SET session_preload_libraries = 'auto_explain'", "permission denied")
	})

	t.Run("names reveal nothing about other projects", func(t *testing.T) {
		// V2 §10.2: database and role names are opaque.
		for _, q := range []string{`SELECT datname FROM pg_database`, `SELECT rolname FROM pg_roles`} {
			rows, err := a.conn.Query(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			list, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range list {
				if strings.Contains(n, "isolation") {
					t.Errorf("%s shows %s", q, n)
				}
			}
		}
		leaks, err := isocheck.Metadata(ctx, a.conn, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range leaks {
			t.Errorf("finding: %s", f)
		}
	})

	t.Run("cannot lift its own limits", func(t *testing.T) {
		// V2 §10.4 quota bypass attempts.
		mustFail(t, a.conn, `SET temp_file_limit = '1TB'`, "permission denied")
		mustFail(t, a.conn, "ALTER ROLE "+pgx.Identifier{a.role()}.Sanitize()+" CONNECTION LIMIT -1", "permission denied")
		mustFail(t, a.conn, "ALTER ROLE "+pgx.Identifier{a.role()}.Sanitize()+" RESET temp_file_limit", "permission denied")
		mustFail(t, a.conn, `CREATE ROLE extra_login LOGIN`, "permission denied")
		var limit string
		if err := a.conn.QueryRow(ctx, `SELECT current_setting('temp_file_limit')`).Scan(&limit); err != nil || limit != "2GB" {
			t.Errorf("temp_file_limit is %q (%v)", limit, err)
		}
	})

	t.Run("guardrails apply", func(t *testing.T) {
		var timeout, idle string
		if err := a.conn.QueryRow(ctx, `SELECT current_setting('statement_timeout'), current_setting('idle_in_transaction_session_timeout')`).Scan(&timeout, &idle); err != nil {
			t.Fatal(err)
		}
		if timeout != "1min" || idle != "1min" {
			t.Errorf("timeouts: statement=%s idle_in_transaction=%s", timeout, idle)
		}
	})
}

// TestClusterConfiguration checks the cluster-wide items of spec §7.1
// with the same checker the weekly live check runs.
func TestClusterConfiguration(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	admin := e.SharedAdmin("postgres")
	findings, err := isocheck.Cluster(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("finding: %s", f)
	}
}

// TestLiveIsolationCheck runs the weekly isolation_check operation through
// the API: it passes on a correctly provisioned cluster, and a violation
// of any kind makes it fail with a finding.
func TestLiveIsolationCheck(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	p := e.CreateProject("audited")

	run := func() gen.Operation {
		t.Helper()
		var ops gen.OperationList
		if code := e.Do("POST", "/api/v1/security/isolation-checks", nil, &ops); code != http.StatusAccepted || len(ops.Items) == 0 {
			t.Fatalf("run checks: %d %+v", code, ops)
		}
		var last gen.Operation
		for _, op := range ops.Items {
			last = e.WaitOperation(op.Id)
			if last.Status != gen.OperationStatusSucceeded {
				return last
			}
		}
		return last
	}
	if op := run(); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("clean cluster: %s %s\n%s", op.Status, deref(op.Error), testenv.FormatLog(op))
	}
	var list gen.IsolationCheckList
	if code := e.Do("GET", "/api/v1/security/isolation-checks", nil, &list); code != http.StatusOK || len(list.Items) == 0 || list.EveryDays != 1 {
		t.Fatalf("list: %d %+v", code, list)
	}
	for _, c := range list.Items {
		if c.Last == nil || c.Last.Status != "succeeded" {
			t.Fatalf("latest check of %s: %+v", c.NodeName, c.Last)
		}
	}
	// The throwaway tenants are gone afterwards.
	admin := e.SharedAdmin("postgres")
	var probes int
	if err := admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_database WHERE datname LIKE 'xenibase\_isocheck\_%') +
		(SELECT count(*) FROM pg_roles WHERE rolname LIKE 'xenibase\_isocheck\_%')`).Scan(&probes); err != nil || probes != 0 {
		t.Fatalf("probe leftovers: %d %v", probes, err)
	}

	// Each violation is found, then undone.
	db := pgx.Identifier{p.Project.DbName}.Sanitize()
	role := pgx.Identifier{p.Project.OwnerRole}.Sanitize()
	projectDB := e.SharedAdmin(p.Project.DbName)
	for _, v := range []struct {
		name, finding string
		conn          *pgx.Conn
		do, undo      string
	}{
		{"PUBLIC may connect", "database grants", admin, "GRANT CONNECT ON DATABASE " + db + " TO PUBLIC", "REVOKE CONNECT ON DATABASE " + db + " FROM PUBLIC"},
		{"role gains CREATEDB", "role attributes", admin, "ALTER ROLE " + role + " CREATEDB", "ALTER ROLE " + role + " NOCREATEDB"},
		{"role joins pg_read_all_data", "role memberships", admin, "GRANT pg_read_all_data TO " + role, "REVOKE pg_read_all_data FROM " + role},
		{"PUBLIC may create in public", "schema public", projectDB, "GRANT CREATE ON SCHEMA public TO PUBLIC", "REVOKE CREATE ON SCHEMA public FROM PUBLIC"},
		{"dblink installed", "escape-prone extensions", projectDB, "CREATE EXTENSION dblink", "DROP EXTENSION dblink"},
		{"PUBLIC may read pg_stat_database", "activity privacy", projectDB, "GRANT SELECT ON pg_catalog.pg_stat_database TO PUBLIC", "REVOKE SELECT ON pg_catalog.pg_stat_database FROM PUBLIC"},
		{"PUBLIC may run pg_stat_get_backend_dbid", "activity privacy", projectDB, "GRANT EXECUTE ON FUNCTION pg_catalog.pg_stat_get_backend_dbid(integer) TO PUBLIC", "REVOKE EXECUTE ON FUNCTION pg_catalog.pg_stat_get_backend_dbid(integer) FROM PUBLIC"},
		// V2 §10.2, §10.4, §10.8.
		{"a descriptive database name", "metadata leak", admin, "CREATE DATABASE customer_invoices", "DROP DATABASE customer_invoices"},
		{"no temp_file_limit", "temp_file_limit", admin, "ALTER ROLE " + role + " RESET temp_file_limit", "ALTER ROLE " + role + " SET temp_file_limit = '2GB'"},
		{"no connection limit", "connection limit", admin, "ALTER ROLE " + role + " CONNECTION LIMIT -1", "ALTER ROLE " + role + " CONNECTION LIMIT 20"},
	} {
		if _, err := v.conn.Exec(ctx, v.do); err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		op := run()
		if _, err := v.conn.Exec(ctx, v.undo); err != nil {
			t.Fatalf("undo %s: %v", v.name, err)
		}
		if op.Status != gen.OperationStatusFailed || !strings.Contains(deref(op.Error), v.finding) {
			t.Errorf("%s: check %s (%s), want a %q finding\n%s", v.name, op.Status, deref(op.Error), v.finding, testenv.FormatLog(op))
		}
	}
	if op := run(); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("after undoing: %s %s", op.Status, deref(op.Error))
	}

	// A suspended organisation's logins must be off (V2 §10.8): suspend,
	// then switch one back on behind Xenibase's back.
	team := e.CreateOrg("Isolated team")
	tp := e.CreateProjectIn("suspended app", team)
	e.Reauth()
	if code := e.Do("POST", "/api/v1/admin/orgs/"+team.String()+"/suspend", map[string]string{"reason": "test"}, nil); code != http.StatusNoContent {
		t.Fatalf("suspend: %d", code)
	}
	if op := run(); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("after suspending: %s %s", op.Status, deref(op.Error))
	}
	tRole := pgx.Identifier{tp.Project.OwnerRole}.Sanitize()
	if _, err := admin.Exec(ctx, "ALTER ROLE "+tRole+" LOGIN"); err != nil {
		t.Fatal(err)
	}
	op := run()
	if op.Status != gen.OperationStatusFailed || !strings.Contains(deref(op.Error), "suspended logins") {
		t.Errorf("a suspended project that can log in: %s (%s)", op.Status, deref(op.Error))
	}
	if _, err := admin.Exec(ctx, "ALTER ROLE "+tRole+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
}

// TestReadOnlyConsoleCannotBecomeOwner: a read-scope token (and so a
// read_only member) runs console SQL as the read-only role, and a
// statement that switches role inside itself cannot reach the owner:
// neither rows RLS hides from <db>_ro nor tables never granted to it.
func TestReadOnlyConsoleCannotBecomeOwner(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	p := e.CreateProject("read-only console")
	conn := e.MustConnect(p.Connection.SessionUrl)
	ro := p.Project.DbName + "_ro"
	read := e.CreateToken(map[string]any{"name": "reader", "org_id": e.OrgID, "scopes": []string{"read"}})
	path := "/api/v1/projects/" + p.Project.Id.String() + "/sql"
	// The read-only role is made with the console's logins, on the first
	// session; the policy below names it.
	yes := true
	if code, raw := e.BearerDo(read, "POST", path, gen.SqlRequest{Query: "SELECT 1", QueryId: uuid.New(), ReadOnly: &yes}, nil); code != http.StatusOK {
		t.Fatalf("first read-only session: status %d %s", code, raw)
	}
	for _, q := range []string{
		`CREATE TABLE t (v text, visible bool)`,
		`INSERT INTO t VALUES ('shown', true), ('rls-hidden', false)`,
		`ALTER TABLE t ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY only_visible ON t FOR SELECT TO ` + pgx.Identifier{ro}.Sanitize() + ` USING (visible)`,
		`GRANT SELECT ON t TO ` + pgx.Identifier{ro}.Sanitize(),
		`CREATE TABLE owner_only (v text)`,
		`INSERT INTO owner_only VALUES ('never-granted')`,
		`REVOKE SELECT ON owner_only FROM ` + pgx.Identifier{ro}.Sanitize(),
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	owner := "'" + p.Project.OwnerRole + "'"
	for _, q := range []string{
		`SELECT set_config('role', ` + owner + `, true), query_to_xml('SELECT * FROM t', true, false, '')`,
		`SELECT set_config('role', ` + owner + `, true), query_to_xml('SELECT * FROM owner_only', true, false, '')`,
		`SET ROLE ` + pgx.Identifier{p.Project.OwnerRole}.Sanitize(),
	} {
		var res gen.SqlResult
		ro := true
		code, raw := e.BearerDo(read, "POST", path, gen.SqlRequest{Query: q, QueryId: uuid.New(), ReadOnly: &ro}, &res)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d %s", q, code, raw)
		}
		if res.Error == nil || deref(res.Error.Code) != "42501" {
			t.Errorf("%s: read-only session was not refused: %s", q, raw)
		}
		if s := string(raw); strings.Contains(s, "rls-hidden") || strings.Contains(s, "never-granted") {
			t.Errorf("%s: read-only session read as the owner: %s", q, raw)
		}
	}
	// The table browser's raw filter runs in the same kind of session.
	where := url.QueryEscape(`set_config('role', ` + owner + `, true) IS NOT NULL AND query_to_xml('SELECT * FROM owner_only', true, false, '')::text <> ''`)
	code, raw := e.BearerDo(read, "GET", "/api/v1/projects/"+p.Project.Id.String()+"/tables/public/t/rows?where="+where, nil, nil)
	if s := string(raw); code == http.StatusOK || strings.Contains(s, "rls-hidden") || strings.Contains(s, "never-granted") {
		t.Errorf("raw filter as the owner: %d %s", code, raw)
	}
	// And the isolation check finds a read-only console login that could.
	admin := e.SharedAdmin("postgres")
	tenant := []isocheck.Tenant{{DB: p.Project.DbName, Role: p.Project.OwnerRole}}
	if f, err := isocheck.Roles(ctx, admin, tenant); err != nil || len(f) > 0 {
		t.Fatalf("clean roles: %v %v", f, err)
	}
	login := pgx.Identifier{p.Project.DbName + "_console_ro"}.Sanitize()
	if _, err := admin.Exec(ctx, "GRANT "+pgx.Identifier{p.Project.OwnerRole}.Sanitize()+" TO "+login+" WITH INHERIT FALSE, SET TRUE"); err != nil {
		t.Fatal(err)
	}
	f, err := isocheck.Roles(ctx, admin, tenant)
	if err != nil || len(f) == 0 || !strings.Contains(f[0].String(), "console logins") {
		t.Errorf("owner granted to the read-only console login: findings %v %v", f, err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func itoa(n int) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}
