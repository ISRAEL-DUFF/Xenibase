package dedicated_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/logical"
	"github.com/israel-duff/xenibase/internal/provision"
)

// moveObjects creates in db what a move named name leaves: its login with
// CONNECT, its publication, a subscription, and the DDL block.
func moveObjects(t *testing.T, cluster, db *pgx.Conn, dbName, name string, withBlock bool) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE ROLE " + provision.Ident(name) + " LOGIN REPLICATION",
		"GRANT CONNECT ON DATABASE " + provision.Ident(dbName) + " TO " + provision.Ident(name),
	} {
		if _, err := cluster.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		"GRANT USAGE ON SCHEMA public TO " + provision.Ident(name),
		"CREATE PUBLICATION " + provision.Ident(name) + " FOR ALL TABLES",
		"CREATE SUBSCRIPTION " + provision.Ident(name) + " CONNECTION 'host=127.0.0.1 port=1 dbname=x' PUBLICATION " + provision.Ident(name) +
			" WITH (connect = false)",
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if withBlock {
		if _, err := db.Exec(ctx, `CREATE SCHEMA `+logical.Schema+`;
CREATE FUNCTION `+logical.Schema+`.block_ddl() RETURNS event_trigger LANGUAGE plpgsql AS $f$
BEGIN
  IF NOT (SELECT rolsuper FROM pg_roles WHERE rolname = session_user) THEN RAISE EXCEPTION 'moving'; END IF;
END $f$;
CREATE EVENT TRIGGER `+logical.Prefix+`ddl_block ON ddl_command_start EXECUTE FUNCTION `+logical.Schema+`.block_ddl()`); err != nil {
			t.Fatal(err)
		}
	}
}

// What a move's cleanup could not remove used to stay for ever: a slot
// keeping WAL, a DDL block refusing the tenant's schema changes. The sweep
// removes what no queued or running operation owns, and leaves a running
// move's objects alone.
func TestSweepMoveLeftovers(t *testing.T) {
	e := setupMove(t, provision.StatusActive)
	ctx := context.Background()
	dbName := e.project.DbName
	if _, err := e.cluster.Exec(ctx, "CREATE DATABASE "+provision.Ident(dbName)); err != nil {
		t.Fatal(err)
	}
	orphan := logical.Name(uuid.NewString())
	live, err := jobs.Enqueue(ctx, e.db, jobs.EnqueueParams{Kind: dedicated.KindMove, ProjectID: &e.project.ID,
		Params: json.RawMessage(`{"source_instance": "` + e.source.ID.String() + `", "target_instance": "` + e.target.ID.String() + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	running := logical.Name(live.ID.String())
	t.Cleanup(func() {
		c := context.Background()
		if db, err := connectDB(e, dbName); err == nil {
			for _, n := range []string{orphan, running} {
				_ = logical.DropSubscription(c, db, n)
				_, _ = db.Exec(c, "DROP OWNED BY "+provision.Ident(n))
			}
			_ = db.Close(c)
		}
		_, _ = e.cluster.Exec(c, "DROP DATABASE IF EXISTS "+provision.Ident(dbName)+" WITH (FORCE)")
		for _, n := range []string{orphan, running} {
			_, _ = e.cluster.Exec(c, "DROP ROLE IF EXISTS "+provision.Ident(n))
		}
	})
	db, err := connectDB(e, dbName)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	moveObjects(t, e.cluster, db, dbName, running, false)
	moveObjects(t, e.cluster, db, dbName, orphan, true)

	found := func() logical.Leftovers {
		t.Helper()
		lo, err := logical.FindLeftovers(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		return lo
	}
	has := func(xs []string, x string) bool {
		for _, y := range xs {
			if y == x {
				return true
			}
		}
		return false
	}
	lo := found()
	if !has(lo.Subscriptions, orphan) || !has(lo.Publications, orphan) || !has(lo.Logins, orphan) || !lo.DDLBlock {
		t.Fatalf("FindLeftovers = %+v, want the orphan's objects", lo)
	}

	// The running move's objects, and the DDL block of the database it
	// moves, stay; the finished one's go.
	if _, err := e.svc.SweepMoveLeftovers(ctx); err != nil {
		t.Fatal(err)
	}
	lo = found()
	if has(lo.Subscriptions, orphan) || has(lo.Publications, orphan) || has(lo.Logins, orphan) || e.roleExists(t, orphan) {
		t.Fatalf("after the sweep: %+v, want the orphan's objects gone", lo)
	}
	if !has(lo.Subscriptions, running) || !has(lo.Publications, running) || !has(lo.Logins, running) || !e.roleExists(t, running) {
		t.Fatalf("after the sweep: %+v, want the running move's objects kept", lo)
	}
	if !lo.DDLBlock {
		t.Fatal("the sweep removed the DDL block of a database a move is copying")
	}

	// Once that move has finished, its objects are leftovers too.
	if _, err := e.db.Exec(ctx, `UPDATE operations SET status = 'succeeded', finished_at = now() WHERE id = $1`, live.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SweepMoveLeftovers(ctx); err != nil {
		t.Fatal(err)
	}
	if lo = found(); len(lo.Subscriptions)+len(lo.Publications)+len(lo.Logins) != 0 || lo.DDLBlock || e.roleExists(t, running) {
		t.Fatalf("after the move finished: %+v, want nothing left", lo)
	}
}

func (e *moveEnv) roleExists(t *testing.T, role string) bool {
	t.Helper()
	var ok bool
	if err := e.cluster.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// connectDB connects to database db on the test server as its superuser.
func connectDB(e *moveEnv, db string) (*pgx.Conn, error) {
	cfg := e.cluster.Config().Copy()
	cfg.Database = db
	return pgx.ConnectConfig(context.Background(), cfg)
}

// No instance capped the WAL a slot may hold, so a move's slot left
// behind kept WAL until the disk filled.
func TestSlotWALIsCapped(t *testing.T) {
	small := provision.Profile{CPUs: 1, MemoryMB: 1024}
	for vol, want := range map[int]string{20: "5120MB", 2: "1024MB", 400: "102400MB"} {
		if got := dedicated.Settings(small, 100, vol)["max_slot_wal_keep_size"]; got != want {
			t.Errorf("a %d GB volume: max_slot_wal_keep_size = %q, want %q", vol, got, want)
		}
	}
	if got := dedicated.SharedSettings(2048)["max_slot_wal_keep_size"]; got == "" || got == "-1" {
		t.Errorf("a shared cluster: max_slot_wal_keep_size = %q, want a bound", got)
	}
}
