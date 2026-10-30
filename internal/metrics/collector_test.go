package metrics

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// testProjects lists the metadata database's projects and points every
// instance at the test server, counting lists and connections opened.
type testProjects struct {
	db     *pgxpool.Pool
	url    string
	none   atomic.Bool // list nothing
	lists  atomic.Int32
	opened atomic.Int32
}

func (p *testProjects) List(ctx context.Context, status *string, limit int) ([]store.Project, error) {
	p.lists.Add(1)
	if p.none.Load() {
		return nil, nil
	}
	return store.New(p.db).ListLiveProjects(ctx, store.ListLiveProjectsParams{Status: status, MaxRows: int32(limit)})
}

func (p *testProjects) AdminConfig(_ context.Context, _ uuid.UUID, database string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(p.url)
	if err != nil {
		return nil, err
	}
	cfg.Database = database
	cfg.RuntimeParams["application_name"] = "xenibase-metrics-test"
	cfg.AfterConnect = func(context.Context, *pgconn.PgConn) error {
		p.opened.Add(1)
		return nil
	}
	return cfg, nil
}

type noPoolers struct{}

func (noPoolers) LocalAdmins() []*pooler.Admin { return nil }
func (noPoolers) HostAdmins() []*pooler.Admin  { return nil }

// setup is a migrated metadata database with one active shared project
// whose database is that metadata database itself, on the test server.
func setup(t *testing.T) (*pgxpool.Pool, *testProjects, store.Project) {
	t.Helper()
	url := os.Getenv(storetest.EnvVar)
	pool := storetest.New(t)
	ctx := context.Background()
	var dbName string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	node, inst, org, proj := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', 'fp', '{}')`, []any{node}},
		{`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, []any{inst, node}},
		{`INSERT INTO organizations (id, name, slug, plan_id) VALUES ($1, 'o', 'o', (SELECT id FROM quota_plans WHERE name = 'Personal'))`, []any{org}},
		{`INSERT INTO projects (id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, org_id, region, last_active_at)
			VALUES ($1, 'p', 'p', $2::text, 'p_owner', 'SCRAM-SHA-256$4096:c2FsdA==$a2V5:a2V5', 'shared', $3, 'active', $4, 'eu-central', now() - interval '30 days')`,
			[]any{proj, dbName, inst, org}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}
	p, err := store.New(pool).GetProject(ctx, proj)
	if err != nil {
		t.Fatal(err)
	}
	return pool, &testProjects{db: pool, url: url}, p
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func pointCount(t *testing.T, db *pgxpool.Pool, metric string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM metric_points WHERE metric = $1`, metric).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Two servers on one metadata database: one collects each round, the
// other only reads its own poolers, and takes over when the first stops.
// Both serve /metrics.
func TestOneCollectorAtATime(t *testing.T) {
	db, ps, p := setup(t)
	ctx := context.Background()
	a := NewCollector(db, ps, nil, nil, time.Minute, quiet())
	b := NewCollector(db, ps, nil, nil, time.Minute, quiet())
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)

	for round := 1; round <= 3; round++ {
		ledA, err := a.Round(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ledB, err := b.Round(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ledA || ledB {
			t.Fatalf("round %d: a led %v, b led %v; want a alone", round, ledA, ledB)
		}
		if got := ps.lists.Load(); got != int32(round) {
			t.Fatalf("round %d: projects listed %d times, want %d (once per round)", round, got, round)
		}
	}
	if n := pointCount(t, db, ConnectionsActive); n == 0 {
		t.Fatal("no points collected")
	}

	// /metrics reads the database, so the server that did not collect
	// serves the latest values too.
	var out bytes.Buffer
	if err := WritePrometheus(ctx, &out, db, time.Minute); err != nil {
		t.Fatal(err)
	}
	if want := `xenibase_project_connections_active{project_id="` + p.ID.String() + `"`; !strings.Contains(out.String(), want) {
		t.Fatalf("/metrics lacks %s:\n%s", want, out.String())
	}

	// The collecting server stops: the other takes over on its next round.
	a.Close()
	ledB, err := b.Round(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ledB {
		t.Fatal("b did not take over after a stopped")
	}
	if ledA, _ := a.Round(ctx); ledA {
		t.Fatal("a collects again while b holds the lock")
	}
}

// The collector keeps its connection to an instance between rounds,
// opens it again when the instance closed it, and closes it when no
// active project is left there.
func TestInstanceConnectionReused(t *testing.T) {
	db, ps, _ := setup(t)
	ctx := context.Background()
	c := NewCollector(db, ps, nil, nil, time.Minute, quiet())
	t.Cleanup(c.Close)
	for range 3 {
		if err := c.Collect(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := ps.opened.Load(); got != 1 {
		t.Fatalf("opened %d instance connections over 3 rounds, want 1", got)
	}

	// The instance drops it (a restart, a failover): the next round
	// reconnects and still records its points.
	if _, err := db.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'xenibase-metrics-test'`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	before := pointCount(t, db, ConnectionsActive)
	if _, err := db.Exec(ctx, `DELETE FROM metric_points`); err != nil {
		t.Fatal(err)
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("collect after the instance closed the connection: %v", err)
	}
	if got := ps.opened.Load(); got != 2 {
		t.Fatalf("opened %d instance connections, want 2", got)
	}
	if before == 0 || pointCount(t, db, ConnectionsActive) == 0 {
		t.Fatal("no points after reconnecting")
	}

	// No active project left on the instance: the connection is closed.
	ps.none.Store(true)
	if err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	var open int
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		// The backend exits shortly after the client closes.
		if err := db.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name = 'xenibase-metrics-test'`).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 || time.Now().After(deadline) {
			break
		}
	}
	if open != 0 || len(c.conns) != 0 {
		t.Fatalf("%d instance connections still open (%d kept), want none", open, len(c.conns))
	}
}

// Another server's reading of its own poolers counts on the collecting
// server: its clients in pooler_clients, and its transaction counter
// moving marks the project active for the Free tier.
func TestOtherServersPoolerReadings(t *testing.T) {
	db, ps, p := setup(t)
	ctx := context.Background()
	c := NewCollector(db, ps, nil, nil, time.Minute, quiet())
	c.pooler = noPoolers{}
	t.Cleanup(c.Close)
	q := store.New(db)
	publish := func(xacts int) {
		t.Helper()
		raw := []byte(`{"` + p.DbName + `":{"c":3,"w":1,"x":` + strconv.Itoa(xacts) + `}}`)
		if err := q.UpsertPoolerReading(ctx, store.UpsertPoolerReadingParams{Source: "other-server/transaction/pooler-tx:6543", Pools: raw}); err != nil {
			t.Fatal(err)
		}
	}
	lastActive := func() time.Time {
		t.Helper()
		var at time.Time
		if err := db.QueryRow(ctx, `SELECT last_active_at FROM projects WHERE id = $1`, p.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	publish(10)
	if err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	var clients, waiting float64
	if err := db.QueryRow(ctx, `SELECT
		(SELECT value FROM metric_points WHERE scope_id = $1 AND metric = 'pooler_clients'),
		(SELECT value FROM metric_points WHERE scope_id = $1 AND metric = 'pooler_waiting')`, p.ID).Scan(&clients, &waiting); err != nil {
		t.Fatal(err)
	}
	if clients != 3 || waiting != 1 {
		t.Fatalf("pooler_clients %v, pooler_waiting %v; want 3 and 1 from the other server's reading", clients, waiting)
	}
	// Clients connected: active.
	if time.Since(lastActive()) > time.Minute {
		t.Fatal("a project with clients on another server's pooler was not marked active")
	}

	// No clients, but transactions moved since the last reading.
	if _, err := db.Exec(ctx, `UPDATE projects SET last_active_at = now() - interval '30 days' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"` + p.DbName + `":{"c":0,"w":0,"x":11}}`)
	if err := q.UpsertPoolerReading(ctx, store.UpsertPoolerReadingParams{Source: "other-server/transaction/pooler-tx:6543", Pools: raw}); err != nil {
		t.Fatal(err)
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(lastActive()) > time.Minute {
		t.Fatal("transactions through another server's pooler did not mark the project active")
	}

	// A stale reading (its server stopped) no longer counts.
	if _, err := db.Exec(ctx, `UPDATE pooler_readings SET sampled_at = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM metric_points`); err != nil {
		t.Fatal(err)
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT value FROM metric_points WHERE scope_id = $1 AND metric = 'pooler_clients'`, p.ID).Scan(&clients); err != nil || clients != 0 {
		t.Fatalf("pooler_clients %v (%v) from a stale reading, want 0", clients, err)
	}
}

// Instance series carry the instance's labels on /metrics.
func TestPrometheusInstanceSeries(t *testing.T) {
	db, _, p := setup(t)
	ctx := context.Background()
	if err := store.New(db).UpsertMetricPoints(ctx, store.UpsertMetricPointsParams{
		Scope: ScopeInstance, ScopeIds: []uuid.UUID{p.InstanceID}, Metrics: []string{WebhookListenConnections}, Ts: bucket(time.Now()), Vals: []float64{7},
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WritePrometheus(ctx, &out, db, time.Minute); err != nil {
		t.Fatal(err)
	}
	want := `xenibase_instance_webhook_listen_connections{instance_id="` + p.InstanceID.String() + `",kind="shared",node_id=`
	if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), `,node="n1"} 7`) {
		t.Fatalf("/metrics lacks the instance series %s...:\n%s", want, out.String())
	}
}
