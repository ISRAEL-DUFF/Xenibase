package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestReadOnlySessionCannotBecomeOwner: a read-only console session's
// login may become the read-only role only, so a statement that switches
// role inside itself (set_config('role', <owner>, true) next to
// query_to_xml) cannot read rows RLS hides from <db>_ro, or tables never
// granted to it. Needs a Postgres 16+ superuser in XENIBASE_TEST_DATABASE_URL.
func TestReadOnlySessionCannotBecomeOwner(t *testing.T) {
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

	b := make([]byte, 5)
	_, _ = rand.Read(b)
	db := "p_rotest" + hex.EncodeToString(b)
	p := store.Project{ID: uuid.New(), DbName: db, OwnerRole: db + "_owner", Tier: provision.TierShared}
	ro := provision.ReadOnlyRole(db)
	id := provision.Ident
	cleanup := func() {
		c := context.Background()
		_, _ = admin.Exec(c, "DROP DATABASE IF EXISTS "+id(db)+" WITH (FORCE)")
		_ = provision.DropConsoleRole(c, admin, provision.ConsoleRole(db), "postgres")
		for _, r := range []string{ro, p.OwnerRole} {
			_, _ = admin.Exec(c, "DROP ROLE IF EXISTS "+id(r))
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	for _, q := range []string{
		"CREATE ROLE " + id(p.OwnerRole) + " LOGIN",
		"CREATE ROLE " + id(ro) + " NOLOGIN NOINHERIT",
		"CREATE DATABASE " + id(db) + " OWNER " + id(p.OwnerRole),
		"REVOKE ALL ON DATABASE " + id(db) + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + id(db) + " TO " + id(ro),
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	cfg, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	dbAdmin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dbAdmin.Close(context.Background())
	for _, q := range []string{
		"SET ROLE " + id(p.OwnerRole),
		"CREATE TABLE t (v text, visible bool)",
		"INSERT INTO t VALUES ('shown', true), ('hidden', false)",
		"ALTER TABLE t ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY only_visible ON t FOR SELECT TO " + id(ro) + " USING (visible)",
		"GRANT SELECT ON t TO " + id(ro),
		"CREATE TABLE owner_only (v text)",
		"INSERT INTO owner_only VALUES ('secret')",
		"RESET ROLE",
	} {
		if _, err := dbAdmin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	ring, err := crypto.NewKeyring(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	prov := provision.NewService(nil, ring, nil, provision.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := prov.ApplyConsoleRoles(ctx, dbAdmin, p); err != nil {
		t.Fatal(err)
	}

	// As connect does: log in, assume the mode's role, read-only.
	session := func(m mode) *pgx.Conn {
		t.Helper()
		c := cfg.Copy()
		c.User = sessionLogin(p, m)
		c.Password = prov.ConsolePassword(c.User)
		conn, err := pgx.ConnectConfig(ctx, c)
		if err != nil {
			t.Fatalf("connect as %s: %v", c.User, err)
		}
		t.Cleanup(func() { _ = conn.Close(context.Background()) })
		assume := p.OwnerRole
		if m == modeReadOnly {
			assume = ro
		}
		if _, err := conn.Exec(ctx, "SET ROLE "+id(assume)+"; BEGIN READ ONLY"); err != nil {
			t.Fatal(err)
		}
		return conn
	}

	conn := session(modeReadOnly)
	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil || n != 1 {
		t.Fatalf("read-only role sees %d rows of t (%v), want 1", n, err)
	}
	for _, q := range []string{
		"SELECT set_config('role', " + "'" + p.OwnerRole + "'" + ", true), query_to_xml('SELECT * FROM t', true, false, '')::text",
		"SELECT set_config('role', " + "'" + p.OwnerRole + "'" + ", true), query_to_xml('SELECT * FROM owner_only', true, false, '')::text",
	} {
		if _, err := conn.Exec(ctx, "SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		var role, out string
		err := conn.QueryRow(ctx, q).Scan(&role, &out)
		if err == nil {
			t.Errorf("read-only session switched to the owner and read: %s", strings.Join(strings.Fields(out), " "))
		} else if !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: unexpected error %v", q, err)
		}
		if _, err := conn.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, "SET ROLE "+id(p.OwnerRole)); err == nil {
		t.Error("read-only session could SET ROLE to the owner")
	}

	// The owner's sessions still become the owner.
	owner := session(modeOwner)
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil || n != 2 {
		t.Fatalf("owner sees %d rows of t (%v), want 2", n, err)
	}
}
