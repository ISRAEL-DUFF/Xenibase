package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/edge"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// dataProject is a project database with backend services' roles and
// schema versions, served by an edge through Explore: the data API end to
// end, without the control plane. Needs a Postgres superuser in
// XENIBASE_TEST_DATABASE_URL.
type dataProject struct {
	t     *testing.T
	ctx   context.Context
	p     store.Project
	admin *pgx.Conn // the project's database, as the superuser
	edge  *edge.Edge
	pc    edgeapi.Project
}

const dataTestPassword = "data-test-password"

func newDataProject(t *testing.T) *dataProject {
	t.Helper()
	adminURL := os.Getenv(storetest.EnvVar)
	if adminURL == "" {
		t.Skipf("%s not set", storetest.EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	root, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	db := "p_data" + hex.EncodeToString(b)
	p := store.Project{ID: uuid.New(), DbName: db, OwnerRole: db + "_owner", Tier: provision.TierShared}
	id := provision.Ident
	roles := append([]string{p.OwnerRole}, store.ServiceRoles(db)...)
	cleanup := func() {
		c := context.Background()
		_, _ = root.Exec(c, "DROP DATABASE IF EXISTS "+id(db)+" WITH (FORCE)")
		for _, r := range roles {
			_, _ = root.Exec(c, "DROP ROLE IF EXISTS "+id(r))
		}
		_ = root.Close(c)
	}
	t.Cleanup(cleanup)
	pw := "PASSWORD '" + dataTestPassword + "'"
	for _, q := range []string{
		"CREATE ROLE " + id(p.OwnerRole) + " LOGIN " + pw,
		"CREATE ROLE " + id(store.AnonRole(db)) + " LOGIN NOINHERIT NOBYPASSRLS " + pw,
		"CREATE ROLE " + id(store.UserRole(db)) + " LOGIN NOINHERIT NOBYPASSRLS " + pw,
		"CREATE ROLE " + id(store.ServiceRole(db)) + " LOGIN NOINHERIT BYPASSRLS " + pw,
		"CREATE ROLE " + id(store.AuthHookRole(db)) + " LOGIN NOINHERIT NOBYPASSRLS " + pw,
		"CREATE ROLE " + id(store.EdgeRole(db)) + " LOGIN NOINHERIT NOBYPASSRLS " + pw,
		"CREATE DATABASE " + id(db) + " OWNER " + id(p.OwnerRole),
	} {
		if _, err := root.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	cfg, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	// The owner owns public, as on a provisioned database.
	if _, err := admin.Exec(ctx, "ALTER SCHEMA public OWNER TO "+id(p.OwnerRole)); err != nil {
		t.Fatal(err)
	}
	logins := map[string]string{}
	for _, r := range store.RequestRoles(db) {
		logins[r] = dataTestPassword
	}
	d := &dataProject{t: t, ctx: ctx, p: p, admin: admin,
		edge: edge.New(edge.Config{Name: "test", Domain: "data.invalid", PoolerAddr: net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))),
			PoolerSSLMode: "disable", CatalogEvery: time.Nanosecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}),
		pc: edgeapi.Project{Ref: "d" + hex.EncodeToString(b)[:7], ProjectID: p.ID, State: edgeapi.StateActive, Version: 1, Database: db,
			EdgeUser: store.EdgeRole(db), Password: dataTestPassword, AnonRole: store.AnonRole(db), UserRole: store.UserRole(db),
			ServiceRole: store.ServiceRole(db), HookRole: store.AuthHookRole(db), Logins: logins, ExposedSchemas: []string{"public"},
			Settings: edgeapi.Settings{StatementTimeoutMs: 8000}},
	}
	t.Cleanup(func() {
		gone := d.pc
		gone.State, gone.Version = edgeapi.StateDisabled, d.pc.Version+1
		d.edge.Explore(context.Background(), gone, "anon", nil, "GET", "/data/v1/x", nil)
	})
	return d
}

// apply runs every schema version, as enable and the reconciler do.
func (d *dataProject) apply() {
	d.t.Helper()
	if err := applyVersionsOn(d.ctx, d.admin, d.p, 0); err != nil {
		d.t.Fatal(err)
	}
	if err := exposeOn(d.ctx, d.admin, d.p, d.pc.ExposedSchemas); err != nil {
		d.t.Fatal(err)
	}
	d.sync()
}

// sync runs xb_auth.sync_grants with the project's public tables, as the
// control plane does after a schema run or a settings change.
func (d *dataProject) sync() {
	d.t.Helper()
	if err := syncGrantsOn(d.ctx, d.admin, d.pc.PublicTables); err != nil {
		d.t.Fatal(err)
	}
}

// exec runs statements as the platform's admin.
func (d *dataProject) exec(stmts ...string) {
	d.t.Helper()
	for _, q := range stmts {
		if _, err := d.admin.Exec(d.ctx, q); err != nil {
			d.t.Fatalf("%s: %v", q, err)
		}
	}
}

// owner runs statements as the project's owner, {{anon}} and its siblings
// filled in.
func (d *dataProject) owner(stmts ...string) {
	d.t.Helper()
	if err := pgx.BeginFunc(d.ctx, d.admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(d.ctx, "SET LOCAL ROLE "+provision.Ident(d.p.OwnerRole)); err != nil {
			return err
		}
		for _, q := range stmts {
			if _, err := tx.Exec(d.ctx, expand(q, d.p)); err != nil {
				d.t.Errorf("%s", q)
				return err
			}
		}
		return nil
	}); err != nil {
		d.t.Fatal(err)
	}
}

// can is has_*_privilege(role, object, priv) for a role placeholder.
func (d *dataProject) can(fn, role, object, priv string) bool {
	d.t.Helper()
	var ok bool
	role = strings.Trim(expand(role, d.p), `"`)
	if err := d.admin.QueryRow(d.ctx, "SELECT "+fn+"($1, $2, $3)", role, object, priv).Scan(&ok); err != nil {
		d.t.Fatalf("%s(%s, %s, %s): %v", fn, role, object, priv, err)
	}
	return ok
}

// call is a data API request as role (anon, user or service).
func (d *dataProject) call(role, method, path, body string) (int, string) {
	d.t.Helper()
	claims := map[string]any{"role": role}
	if role == "user" {
		claims["sub"] = "00000000-0000-4000-8000-000000000001"
	}
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	code, _, out := d.edge.Explore(d.ctx, d.pc, role, claims, method, path, b)
	return code, string(out)
}

// expect checks a request's status.
func (d *dataProject) expect(want int, role, method, path, body string) string {
	d.t.Helper()
	code, out := d.call(role, method, path, body)
	if code != want {
		d.t.Fatalf("%s %s %s as %s: %d %s, want %d", method, path, body, role, code, out, want)
	}
	return out
}
