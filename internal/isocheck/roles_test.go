package isocheck

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// liveTenant makes a project database on the test cluster as provisioning
// leaves it, with its five backend-service logins as ensureRolesOn creates
// them on the shared tier, and returns it with a superuser connection.
func liveTenant(ctx context.Context, t *testing.T, admin *pgx.Conn) Tenant {
	t.Helper()
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	db := "p_isotest" + hex.EncodeToString(b)
	owner := provision.OwnerRoleName(db)
	id := provision.Ident
	roles := append([]string{owner}, store.ServiceRoles(db)...)
	t.Cleanup(func() {
		c := context.Background()
		a, err := pgx.Connect(c, os.Getenv(storetest.EnvVar))
		if err != nil {
			return
		}
		defer a.Close(c)
		_, _ = a.Exec(c, "DROP DATABASE IF EXISTS "+id(db)+" WITH (FORCE)")
		for _, r := range roles {
			_, _ = a.Exec(c, "DROP ROLE IF EXISTS "+id(r))
		}
	})
	stmts := []string{
		"CREATE ROLE " + id(owner) + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 20",
		"ALTER ROLE " + id(owner) + " SET temp_file_limit = '" + provision.TempFileLimit + "'",
		"CREATE DATABASE " + id(db) + " OWNER " + id(owner) + " TEMPLATE template0",
		"REVOKE ALL ON DATABASE " + id(db) + " FROM PUBLIC",
		"GRANT CONNECT, TEMPORARY ON DATABASE " + id(db) + " TO " + id(owner),
	}
	for _, r := range store.ServiceRoles(db) {
		bypass := "NOBYPASSRLS"
		if r == store.ServiceRole(db) {
			bypass = "BYPASSRLS"
		}
		stmts = append(stmts,
			"CREATE ROLE "+id(r)+" LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION "+bypass+" CONNECTION LIMIT 10",
			"ALTER ROLE "+id(r)+" SET temp_file_limit = '"+provision.TempFileLimit+"'",
			"GRANT CONNECT ON DATABASE "+id(db)+" TO "+id(r))
	}
	for _, q := range stmts {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return Tenant{DB: db, Role: owner}
}

// TestRolesServiceLoginsAndGrants: Roles audits the five backend-service
// logins as it does the owner (attributes, limits, memberships, NOLOGIN
// while suspended) and requires every holder of CONNECT or TEMPORARY on a
// project database to be one of that project's roles. Before, a request
// role could be broken in any of these ways, and another project's role
// could be granted CONNECT, without a finding.
func TestRolesServiceLoginsAndGrants(t *testing.T) {
	adminURL := os.Getenv(storetest.EnvVar)
	if adminURL == "" {
		t.Skipf("%s not set", storetest.EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	a := liveTenant(ctx, t, admin)
	b := liveTenant(ctx, t, admin)
	id := provision.Ident

	findings := func(tn Tenant) []Finding {
		t.Helper()
		fs, err := Roles(ctx, admin, []Tenant{tn})
		if err != nil {
			t.Fatal(err)
		}
		return fs
	}
	if fs := findings(a); len(fs) != 0 {
		t.Fatalf("a tenant as provisioned: %v", fs)
	}
	exec := func(q string) {
		t.Helper()
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	anon, edge, service := store.AnonRole(a.DB), store.EdgeRole(a.DB), store.ServiceRole(a.DB)
	for _, c := range []struct {
		name, check, breakIt, fix string
	}{
		{"an attribute", "role attributes", "ALTER ROLE " + id(anon) + " CREATEDB", "ALTER ROLE " + id(anon) + " NOCREATEDB"},
		{"BYPASSRLS outside the service role", "role attributes", "ALTER ROLE " + id(edge) + " BYPASSRLS", "ALTER ROLE " + id(edge) + " NOBYPASSRLS"},
		{"no connection limit", "connection limit", "ALTER ROLE " + id(service) + " CONNECTION LIMIT -1", "ALTER ROLE " + id(service) + " CONNECTION LIMIT 10"},
		{"no temp_file_limit", "temp_file_limit", "ALTER ROLE " + id(anon) + " RESET temp_file_limit",
			"ALTER ROLE " + id(anon) + " SET temp_file_limit = '" + provision.TempFileLimit + "'"},
		{"a predefined role", "role memberships", "GRANT pg_read_all_stats TO " + id(anon), "REVOKE pg_read_all_stats FROM " + id(anon)},
		{"the edge able to become a request role", "service logins", "GRANT " + id(service) + " TO " + id(edge), "REVOKE " + id(service) + " FROM " + id(edge)},
		{"a request role able to become the owner", "service logins", "GRANT " + id(a.Role) + " TO " + id(anon), "REVOKE " + id(a.Role) + " FROM " + id(anon)},
		{"LOGIN while suspended", "suspended logins", "SELECT 1", "SELECT 1"},
		{"another project's role with CONNECT", "database grants", "GRANT CONNECT ON DATABASE " + id(a.DB) + " TO " + id(b.Role),
			"REVOKE CONNECT ON DATABASE " + id(a.DB) + " FROM " + id(b.Role)},
		{"another project's request role with TEMPORARY", "database grants", "GRANT TEMPORARY ON DATABASE " + id(a.DB) + " TO " + id(store.AnonRole(b.DB)),
			"REVOKE TEMPORARY ON DATABASE " + id(a.DB) + " FROM " + id(store.AnonRole(b.DB))},
		{"PUBLIC with CONNECT", "database grants", "GRANT CONNECT ON DATABASE " + id(a.DB) + " TO PUBLIC", "REVOKE CONNECT ON DATABASE " + id(a.DB) + " FROM PUBLIC"},
	} {
		exec(c.breakIt)
		tn := a
		suspended := c.check == "suspended logins"
		if suspended {
			// The owner is switched off; the service logins are not.
			exec("ALTER ROLE " + id(a.Role) + " NOLOGIN")
			tn.LoginsOff = true
		}
		fs := findings(tn)
		var hit bool
		for _, f := range fs {
			hit = hit || f.Check == c.check
		}
		if !hit {
			t.Errorf("%s: no %q finding in %v", c.name, c.check, fs)
		}
		exec(c.fix)
		if suspended {
			exec("ALTER ROLE " + id(a.Role) + " LOGIN")
		}
		if fs := findings(a); len(fs) != 0 {
			t.Fatalf("after undoing %s: %v", c.name, fs)
		}
	}
	// Suspended with every login off, the service logins included: clean.
	for _, r := range append([]string{a.Role}, store.ServiceRoles(a.DB)...) {
		exec("ALTER ROLE " + id(r) + " NOLOGIN")
	}
	off := a
	off.LoginsOff = true
	if fs := findings(off); len(fs) != 0 {
		t.Fatalf("a suspended project with its logins off: %v", fs)
	}
}

// TestSecurityDefiners: a SECURITY DEFINER function owned by a role that
// is not the tenant's (a superuser) is a finding; Xenibase's own in xb_*
// with a pinned search_path, and the tenant's own, are not.
func TestSecurityDefiners(t *testing.T) {
	adminURL := os.Getenv(storetest.EnvVar)
	if adminURL == "" {
		t.Skipf("%s not set", storetest.EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	tn := liveTenant(ctx, t, admin)
	cfg, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = tn.DB
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	exec := func(q string) {
		t.Helper()
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	finding := func() string {
		t.Helper()
		fs, err := Database(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fs {
			if f.Check == "security definer" {
				return f.Detail
			}
		}
		return ""
	}
	exec(`CREATE SCHEMA xb_auth`)
	exec(`CREATE FUNCTION xb_auth.ok() RETURNS int LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS 'SELECT 1'`)
	exec(`CREATE FUNCTION public.mine() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`)
	exec(`ALTER FUNCTION public.mine() OWNER TO ` + provision.Ident(tn.Role))
	if f := finding(); f != "" {
		t.Fatalf("Xenibase's own and the tenant's functions: %s", f)
	}
	exec(`CREATE FUNCTION public.escalate() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`)
	if f := finding(); !strings.Contains(f, "public.escalate()") {
		t.Fatalf("a superuser's SECURITY DEFINER function in public: %q", f)
	}
	exec(`DROP FUNCTION public.escalate()`)
	exec(`CREATE FUNCTION xb_auth.unpinned() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`)
	if f := finding(); !strings.Contains(f, "xb_auth.unpinned()") {
		t.Fatalf("a platform function without a pinned search_path: %q", f)
	}
}
