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
