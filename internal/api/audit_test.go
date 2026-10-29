package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// flakyAuditDB refuses the next `fail` audit_log inserts, as a metadata
// database does during a failover.
type flakyAuditDB struct {
	*pgxpool.Pool
	fail atomic.Int32
}

func (d *flakyAuditDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO audit_log") && d.fail.Add(-1) >= 0 {
		return pgconn.CommandTag{}, errors.New("the database system is shutting down")
	}
	return d.Pool.Exec(ctx, sql, args...)
}

// TestAuditRowSurvivesAFailedWrite: a mutating request whose audit insert
// fails is still audited, once the database takes writes again, with the
// time of the request.
func TestAuditRowSurvivesAFailedWrite(t *testing.T) {
	pool := storetest.New(t)
	db := &flakyAuditDB{Pool: pool}
	db.fail.Store(1)
	key, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(key)
	clk := &fakeClock{t: time.Now()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.NewService(pool, kr, auth.Config{Now: clk.Now, ReauthWindow: time.Minute}, "let-me-in", log)
	if err := svc.EnsureTerms(context.Background()); err != nil {
		t.Fatal(err)
	}
	orgSvc := orgs.New(pool, svc, nil, nil, "", log)
	svc.SetHooks(orgSvc.Hooks())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ts := httptest.NewServer(NewHandler(Options{
		Logger: log, DB: db, Auth: svc, Orgs: orgSvc, StreamCtx: ctx,
		UI: fstest.MapFS{"index.html": {Data: []byte("app")}}, UIIndex: "index.html",
	}))
	t.Cleanup(ts.Close)

	start := time.Now()
	newBrowser(t, ts.URL).setup(clk) // setup.begin is refused; setup.complete queues behind it
	if db.fail.Load() > 0 {
		t.Fatalf("the audit inserts were not attempted: %d", db.fail.Load())
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		var first time.Time
		if err := pool.QueryRow(context.Background(), `SELECT count(*), coalesce(min(created_at), now()) FROM audit_log
			WHERE action IN ('setup.begin', 'setup.complete')`).Scan(&n, &first); err != nil {
			t.Fatal(err)
		}
		if n == 2 {
			if first.Before(start.Add(-time.Second)) || first.After(start.Add(5*time.Second)) {
				t.Fatalf("retried row's time %v, request at %v", first, start)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit rows after a failed write: %d of 2", n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
