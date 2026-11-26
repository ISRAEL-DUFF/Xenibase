package tenancy_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store/storetest"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// TestReaperEndsStatementThatCatchesCancel: a PL/pgSQL block that traps
// query_canceled survives the reaper's cancel and keeps its query_start.
// On the next pass the reaper finds the same backend still running the
// same statement and terminates it, recorded as statement_terminated.
// Before, it cancelled it again on every pass and the session never ended.
func TestReaperEndsStatementThatCatchesCancel(t *testing.T) {
	if os.Getenv(storetest.EnvVar) == "" {
		t.Skipf("%s not set", storetest.EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, s := newCluster(t, tenancy.Config{StatementLimit: 500 * time.Millisecond})
	tn := c.tenant(t)

	conn, err := pgx.ConnectConfig(ctx, tn.conn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var pid int32
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, `DO $$ BEGIN LOOP BEGIN PERFORM pg_sleep(600); EXCEPTION WHEN query_canceled THEN NULL; END; END LOOP; END $$`)
		done <- err
	}()

	backend := func() (alive bool, start time.Time) {
		t.Helper()
		err := c.admin.QueryRow(ctx, `SELECT query_start FROM pg_stat_activity WHERE pid = $1 AND state = 'active'`, pid).Scan(&start)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, start
		}
		if err != nil {
			t.Fatal(err)
		}
		return true, start
	}
	time.Sleep(time.Second)
	_, started := backend()

	kinds := func() []string {
		t.Helper()
		rows, err := c.db.Query(ctx, `SELECT kind FROM reaped_sessions WHERE project_id = $1 ORDER BY id`, tn.id)
		if err != nil {
			t.Fatal(err)
		}
		ks, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return ks
	}

	if _, err := s.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	alive, again := backend()
	if !alive || !again.Equal(started) {
		t.Fatalf("the block did not catch the cancel (alive %v, query_start %v then %v); the test proves nothing", alive, started, again)
	}
	if ks := kinds(); len(ks) != 1 || ks[0] != "statement" {
		t.Fatalf("after the first pass: %v", ks)
	}

	got, err := s.Reap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "statement_terminated" {
		t.Fatalf("second pass: %+v", got)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the block finished normally")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the session that caught the cancel is still running")
	}
	if ks := kinds(); len(ks) != 2 || ks[1] != "statement_terminated" {
		t.Fatalf("reaped log: %v", ks)
	}
	var pidLogged int32
	var startLogged time.Time
	if err := c.db.QueryRow(ctx, `SELECT pid, started_at FROM reaped_sessions WHERE project_id = $1 AND kind = 'statement'`, tn.id).Scan(&pidLogged, &startLogged); err != nil {
		t.Fatal(err)
	}
	if pidLogged != pid || !startLogged.Equal(started) {
		t.Fatalf("logged pid %d and start %v, want %d and %v", pidLogged, startLogged, pid, started)
	}
}
