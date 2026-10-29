package tenancy_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store/storetest"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// TestSoftLockedGrowthIsHardLocked: a soft-locked tenant that keeps
// writing in explicit read-write transactions (which the soft lock
// allows) is measured on the next enforcement pass, without waiting for
// the 5-minute size sample, and hard-locked once it has grown by 5% of its
// limit; the hard lock holds until it is back under the limit. Before,
// enforcement used only the sample, so it stayed soft-locked and kept
// filling the disk until the next sample.
func TestSoftLockedGrowthIsHardLocked(t *testing.T) {
	if os.Getenv(storetest.EnvVar) == "" {
		t.Skipf("%s not set", storetest.EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, s := newCluster(t, tenancy.Config{})
	tn := c.tenant(t)

	size := func() int64 {
		t.Helper()
		var n int64
		if err := c.admin.QueryRow(ctx, `SELECT pg_database_size($1)`, tn.db).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	limitMB := size()>>20 + 16
	limit := limitMB << 20
	if _, err := c.db.Exec(ctx, `UPDATE organizations SET limit_overrides = jsonb_build_object('project_storage_mb', $2::int) WHERE id = $1`, c.org, limitMB); err != nil {
		t.Fatal(err)
	}
	connect := func() *pgx.Conn {
		t.Helper()
		conn, err := pgx.ConnectConfig(ctx, tn.conn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		// Writes in read-write transactions, as the soft lock allows.
		if _, err := conn.Exec(ctx, `SET default_transaction_read_only = off`); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	app := connect()
	if _, err := app.Exec(ctx, `CREATE TABLE fill (pad text)`); err != nil {
		t.Fatal(err)
	}
	fillTo := func(conn *pgx.Conn, target int64) {
		t.Helper()
		for size() < target {
			if _, err := conn.Exec(ctx, `INSERT INTO fill SELECT repeat(md5(g::text), 8) FROM generate_series(1, 1000) g`); err != nil {
				t.Fatalf("fill: %v", err)
			}
		}
	}
	// The 5-minute sample, as metrics.CollectSizes records it.
	sample := func() {
		t.Helper()
		if _, err := c.db.Exec(ctx, `INSERT INTO metric_points (scope, scope_id, metric, ts, resolution, value)
			VALUES ('project', $1, 'size_bytes', now(), '1m', $2)`, tn.id, size()); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		var st string
		if err := c.db.QueryRow(ctx, `SELECT storage_state FROM projects WHERE id = $1`, tn.id).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	enforce := func() {
		t.Helper()
		if err := s.EnforceStorage(ctx); err != nil {
			t.Fatal(err)
		}
	}

	fillTo(app, limit+limit/50)
	sample()
	enforce()
	if st := state(); st != tenancy.StateSoft {
		t.Fatalf("at 102%%: %s", st)
	}
	writer := connect() // the soft lock ended idle sessions
	enforce()
	if st := state(); st != tenancy.StateSoft {
		t.Fatalf("soft-locked and not growing: %s", st)
	}
	// No new sample: the next pass measures the database itself.
	fillTo(writer, limit+limit*8/100)
	enforce()
	if st := state(); st != tenancy.StateHard {
		t.Fatalf("grown from 102%% to %d%% while soft-locked: %s", size()*100/limit, st)
	}
	if err := writer.Ping(ctx); err == nil {
		t.Fatal("the writer's session survived the hard lock")
	}
	var canLogin bool
	if err := c.admin.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, tn.conn.User).Scan(&canLogin); err != nil {
		t.Fatal(err)
	}
	if canLogin {
		t.Fatal("the owner can still log in while hard-locked")
	}
	// Under 120% is not under the limit: the hard lock holds.
	enforce()
	if st := state(); st != tenancy.StateHard {
		t.Fatalf("a hard-locked project at %d%%: %s", size()*100/limit, st)
	}
	// Cleaned up from the console (an admin connection here): unlocked.
	cfg := c.cfg.Copy()
	cfg.Database = tn.db
	cfg.RuntimeParams["default_transaction_read_only"] = "off"
	clean, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close(context.Background())
	if _, err := clean.Exec(ctx, `DROP TABLE fill`); err != nil {
		t.Fatal(err)
	}
	sample()
	enforce()
	if st := state(); st != tenancy.StateNone {
		t.Fatalf("after cleaning up (%d%%): %s", size()*100/limit, st)
	}
	if err := c.admin.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, tn.conn.User).Scan(&canLogin); err != nil {
		t.Fatal(err)
	}
	if !canLogin {
		t.Fatal("the owner's login stayed off after the unlock")
	}
}
