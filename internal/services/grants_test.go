package services

import (
	"net/http"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/datacat"
)

// TestExtensionFunctionsStayLocked: an extension's function locked from
// PUBLIC, pg_stat_statements_reset, which empties every database's
// statistics, is not granted to the request roles by version 1, is taken
// back from a project an earlier release granted it to, and the data API
// neither lists nor calls it. The project's own functions are granted as
// before, now and when made later.
func TestExtensionFunctionsStayLocked(t *testing.T) {
	d := newDataProject(t)
	d.exec(`CREATE EXTENSION pg_stat_statements SCHEMA public`)
	d.owner(`CREATE FUNCTION public.hello(name text DEFAULT 'you') RETURNS text LANGUAGE sql STABLE AS $$ SELECT 'hello ' || name $$`)
	const reset = "public.pg_stat_statements_reset(oid, oid, bigint)"
	d.apply()
	for _, role := range []string{"{{anon}}", "{{user}}", "{{service}}"} {
		if d.can("has_function_privilege", role, reset, "EXECUTE") {
			t.Errorf("%s may run pg_stat_statements_reset after version 1", role)
		}
		if !d.can("has_function_privilege", role, "public.hello(text)", "EXECUTE") {
			t.Errorf("%s may not run the owner's function", role)
		}
	}
	// A project an earlier release enabled holds the grant: version 7 takes
	// it back.
	d.exec(expand(`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO {{anon}}, {{user}}, {{service}}`, d.p))
	if !d.can("has_function_privilege", "{{anon}}", reset, "EXECUTE") {
		t.Fatal("the old grant didn't reach the extension's function")
	}
	if err := applyVersionsOn(d.ctx, d.admin, d.p, 6); err != nil {
		t.Fatal(err)
	}
	if d.can("has_function_privilege", "{{anon}}", reset, "EXECUTE") || d.can("has_function_privilege", "{{service}}", reset, "EXECUTE") {
		t.Fatal("version 7 left the extension's function granted")
	}
	// The owner's later functions still reach the roles.
	d.owner(`CREATE FUNCTION public.later() RETURNS int LANGUAGE sql STABLE AS $$ SELECT 1 $$`)
	if !d.can("has_function_privilege", "{{anon}}", "public.later()", "EXECUTE") {
		t.Fatal("default privileges no longer grant the owner's new functions")
	}

	cat, err := datacat.Introspect(d.ctx, d.admin, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	for k := range cat.Functions {
		if strings.HasPrefix(k, "public.pg_stat_statements") {
			t.Errorf("the catalog lists %s", k)
		}
	}
	if cat.Functions["public.hello"] == nil {
		t.Error("the catalog lost the owner's function")
	}
	d.expect(http.StatusNotFound, "anon", "POST", "/data/v1/rpc/pg_stat_statements_reset", "{}")
	d.expect(http.StatusNotFound, "service", "POST", "/data/v1/rpc/pg_stat_statements_reset", "{}")
	if out := d.expect(http.StatusOK, "anon", "POST", "/data/v1/rpc/hello", `{"name":"Ada"}`); !strings.Contains(out, "hello Ada") {
		t.Fatalf("hello: %s", out)
	}
}

// TestRLSGateReachesViewsAndFunctions: a table without row-level security
// is out of reach of the publishable key whichever way it is asked for:
// directly, through a security_invoker view, or through a function that
// runs as the caller. anon and user lose their privileges on it as soon as
// the edge sees it, and get them back once it has row-level security, so
// views and functions over protected tables keep working; public tables
// stay readable but not writable.
func TestRLSGateReachesViewsAndFunctions(t *testing.T) {
	d := newDataProject(t)
	d.pc.PublicTables = []string{"countries"}
	d.apply()
	d.owner(
		`CREATE TABLE payments (id int PRIMARY KEY, amount int)`,
		`INSERT INTO payments VALUES (1, 100), (2, 250)`,
		`CREATE VIEW payment_totals WITH (security_invoker = true) AS SELECT sum(amount) AS total FROM payments`,
		`CREATE FUNCTION search_payments() RETURNS SETOF payments LANGUAGE sql STABLE AS $$ SELECT * FROM payments $$`,
		`CREATE FUNCTION payments_total() RETURNS bigint LANGUAGE plpgsql STABLE AS $$ BEGIN RETURN (SELECT sum(amount) FROM payments); END $$`,
		`CREATE TABLE todos (id int PRIMARY KEY, title text)`,
		`INSERT INTO todos VALUES (1, 'milk')`,
		`CREATE VIEW todo_titles WITH (security_invoker = true) AS SELECT id, title FROM todos`,
		`CREATE FUNCTION list_todos() RETURNS SETOF todos LANGUAGE sql STABLE AS $$ SELECT * FROM todos $$`,
		`CREATE TABLE countries (code text PRIMARY KEY)`,
		`INSERT INTO countries VALUES ('NG')`,
		`CREATE FUNCTION add_country(code text) RETURNS void LANGUAGE sql AS $$ INSERT INTO countries VALUES (code) $$`,
		`CREATE FUNCTION count_countries() RETURNS bigint LANGUAGE sql STABLE AS $$ SELECT count(*) FROM countries $$`,
	)
	if !d.can("has_table_privilege", "{{anon}}", "payments", "SELECT") {
		t.Fatal("the default privileges didn't grant the new table")
	}
	// The first request sees the new tables: nothing protects payments
	// and todos yet, so anon and user lose them.
	d.expect(http.StatusForbidden, "anon", "GET", "/data/v1/payments", "")
	for _, role := range []string{"{{anon}}", "{{user}}"} {
		for _, tbl := range []string{"payments", "todos"} {
			if d.can("has_any_column_privilege", role, tbl, "SELECT, INSERT, UPDATE") || d.can("has_table_privilege", role, tbl, "DELETE") {
				t.Errorf("%s still holds privileges on %s, which has no row-level security", role, tbl)
			}
		}
	}
	if !d.can("has_table_privilege", "{{anon}}", "countries", "SELECT") || d.can("has_table_privilege", "{{anon}}", "countries", "INSERT") {
		t.Error("a public table should stay readable and stop being writable")
	}
	if !d.can("has_table_privilege", "{{service}}", "payments", "SELECT") {
		t.Error("the service role lost a table")
	}
	for _, role := range []string{"anon", "user"} {
		out := d.expect(http.StatusForbidden, role, "GET", "/data/v1/payment_totals", "")
		if !strings.Contains(out, "rls_required") || !strings.Contains(out, "public.payments") {
			t.Errorf("an invoker view over payments as %s: %s", role, out)
		}
		d.expect(http.StatusForbidden, role, "POST", "/data/v1/rpc/search_payments", "{}")
		d.expect(http.StatusForbidden, role, "POST", "/data/v1/rpc/payments_total", "{}")
		d.expect(http.StatusForbidden, role, "POST", "/data/v1/rpc/add_country", `{"code":"GH"}`)
		if out := d.expect(http.StatusOK, role, "POST", "/data/v1/rpc/count_countries", "{}"); !strings.Contains(out, `"data":1`) {
			t.Errorf("a function reading a public table: %s", out)
		}
		d.expect(http.StatusOK, role, "GET", "/data/v1/countries", "")
	}
	d.expect(http.StatusOK, "service", "GET", "/data/v1/payment_totals", "")
	d.expect(http.StatusOK, "service", "POST", "/data/v1/rpc/search_payments", "{}")

	// todos gets row-level security and a policy: its privileges come back,
	// and the view and the function work as before.
	d.owner(`ALTER TABLE todos ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY readable ON todos FOR SELECT TO {{anon}}, {{user}} USING (true)`)
	for _, path := range []string{"/data/v1/todos", "/data/v1/todo_titles"} {
		if out := d.expect(http.StatusOK, "anon", "GET", path, ""); !strings.Contains(out, "milk") {
			t.Errorf("%s: %s", path, out)
		}
	}
	if out := d.expect(http.StatusOK, "anon", "POST", "/data/v1/rpc/list_todos", "{}"); !strings.Contains(out, "milk") {
		t.Errorf("list_todos: %s", out)
	}
	if !d.can("has_table_privilege", "{{user}}", "todos", "DELETE") {
		t.Error("the user role didn't get todos back")
	}
	// Turned off again, it is out of reach again.
	d.owner(`ALTER TABLE todos DISABLE ROW LEVEL SECURITY`)
	d.expect(http.StatusForbidden, "anon", "GET", "/data/v1/todo_titles", "")
	if d.can("has_table_privilege", "{{anon}}", "todos", "SELECT") {
		t.Error("anon kept todos after row-level security went off")
	}

	// A project whose database can't sync (behind version 7): the gates
	// alone refuse what anon can reach unprotected.
	d.owner(`ALTER TABLE todos ENABLE ROW LEVEL SECURITY`)
	d.expect(http.StatusOK, "anon", "POST", "/data/v1/rpc/list_todos", "{}")
	d.exec(`DROP FUNCTION xb_auth.sync_grants(text[])`, expand(`GRANT SELECT ON payments TO {{anon}}`, d.p))
	out := d.expect(http.StatusForbidden, "anon", "POST", "/data/v1/rpc/list_todos", "{}")
	if !strings.Contains(out, "public.payments") {
		t.Errorf("the function gate: %s", out)
	}
	d.expect(http.StatusOK, "service", "POST", "/data/v1/rpc/list_todos", "{}")

	// The advisor says so.
	found, err := advise(d.ctx, d.admin, []string{"public"}, d.pc.PublicTables, d.pc.AnonRole)
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]string{}
	for _, f := range found {
		codes[f.Code+" "+f.Object] = f.Message
	}
	if m := codes["rls_disabled public.payments"]; !strings.Contains(m, "through a view or function") {
		t.Errorf("rls_disabled: %q", m)
	}
	if _, ok := codes["view_reads_unprotected public.payment_totals"]; !ok {
		t.Errorf("no view_reads_unprotected finding: %v", codes)
	}
}
