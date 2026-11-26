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
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestActivityPrivacy: after provision.RestrictActivity a tenant in its
// own database can no longer see other databases' activity
// (pg_stat_database, pg_stat_database_conflicts) or other backends'
// database and role (pg_stat_get_backend_*), and Database reports a
// database where it can. Needs a superuser in XENIBASE_TEST_DATABASE_URL.
func TestActivityPrivacy(t *testing.T) {
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
	db := "p_acttest" + hex.EncodeToString(b)
	role := db + "_owner"
	id := provision.Ident
	t.Cleanup(func() {
		c := context.Background()
		_, _ = admin.Exec(c, "DROP DATABASE IF EXISTS "+id(db)+" WITH (FORCE)")
		_, _ = admin.Exec(c, "DROP ROLE IF EXISTS "+id(role))
	})
	for _, q := range []string{
		"CREATE ROLE " + id(role) + " LOGIN PASSWORD 'tenant-pw'",
		"CREATE DATABASE " + id(db) + " OWNER " + id(role),
		"REVOKE ALL ON DATABASE " + id(db) + " FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + id(db) + " TO " + id(role),
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
	tc := cfg.Copy()
	tc.User, tc.Password = role, "tenant-pw"

	finding := func() string {
		t.Helper()
		fs, err := Database(ctx, dbAdmin)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fs {
			if f.Check == "activity privacy" {
				return f.Detail
			}
		}
		return ""
	}
	if finding() == "" {
		t.Fatal("a database with the default ACLs has no activity finding")
	}

	if err := provision.RestrictActivity(ctx, dbAdmin); err != nil {
		t.Fatal(err)
	}
	if f := finding(); f != "" {
		t.Errorf("after RestrictActivity: %s", f)
	}
	tenant, err := pgx.ConnectConfig(ctx, tc)
	if err != nil {
		t.Fatal(err)
	}
	defer tenant.Close(context.Background())
	for _, q := range []string{
		`SELECT count(*) FROM pg_stat_activity`,
		`SELECT count(*) FROM pg_stat_database`,
		`SELECT count(*) FROM pg_stat_database_conflicts`,
		`SELECT pg_stat_get_db_xact_commit(oid) FROM pg_database LIMIT 1`,
		`SELECT pg_stat_get_db_numbackends(oid) FROM pg_database LIMIT 1`,
		`SELECT pg_stat_get_backend_dbid(1)`,
		`SELECT pg_stat_get_backend_userid(1)`,
		`SELECT count(*) FROM pg_stat_get_backend_idset()`,
		`SELECT count(*) FROM pg_locks`,
		`SELECT count(*) FROM pg_lock_status()`,
		`SELECT pg_blocking_pids(pg_backend_pid())`,
		`SELECT pg_safe_snapshot_blocking_pids(pg_backend_pid())`,
		`SELECT count(*) FROM pg_prepared_xacts`,
	} {
		var v any
		err := tenant.QueryRow(ctx, q).Scan(&v)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s as a tenant: %v %v, want permission denied", q, v, err)
		}
	}
	// It is idempotent, for databases brought up to date by apply_settings.
	if err := provision.RestrictActivity(ctx, dbAdmin); err != nil {
		t.Fatal(err)
	}
}
