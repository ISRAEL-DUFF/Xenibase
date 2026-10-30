package webhooks

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// fakeProject is a project whose database is a throwaway database on the
// test server, with an outbox the fake pass drains.
type fakeProject struct {
	p   store.Project
	db  *pgxpool.Pool
	cfg *pgx.ConnConfig
}

// delivery is one event the fake pass sent, and whether its loop held a
// LISTEN connection then.
type delivery struct {
	id        int64
	listening bool
}

type listenRig struct {
	t        *testing.T
	s        *Service
	inst     uuid.UUID
	projects map[uuid.UUID]*fakeProject

	mu       sync.Mutex
	sent     map[uuid.UUID][]delivery
	maxHeld  int
	cancel   context.CancelFunc
	finished sync.WaitGroup
}

func newListenRig(t *testing.T, cfg Config, n int) *listenRig {
	t.Helper()
	r := &listenRig{t: t, inst: uuid.New(), projects: map[uuid.UUID]*fakeProject{}, sent: map[uuid.UUID][]delivery{}}
	ctx := context.Background()
	for range n {
		db := storetest.NewEmpty(t)
		if _, err := db.Exec(ctx, `CREATE SCHEMA xenibase; CREATE TABLE xenibase.webhook_outbox (id bigserial PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		cfg := db.Config().ConnConfig.Copy()
		cfg.RuntimeParams["application_name"] = "xenibase-webhooks-test"
		p := store.Project{ID: uuid.New(), InstanceID: r.inst, DbName: cfg.Database, Status: provision.StatusActive}
		r.projects[p.ID] = &fakeProject{p: p, db: db, cfg: cfg}
	}
	r.s = New(nil, nil, nil, nil, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.s.loop = loopDeps{
		project: func(_ context.Context, id uuid.UUID) (store.Project, error) { return r.projects[id].p, nil },
		connect: func(ctx context.Context, p store.Project) (*pgx.Conn, error) {
			return pgx.ConnectConfig(ctx, r.projects[p.ID].cfg)
		},
		pass:   r.pass,
		health: func(context.Context, *pgx.Conn, store.Project) {},
	}
	return r
}

// pass sends every event in the outbox, in order, as deliverProject does.
func (r *listenRig) pass(ctx context.Context, conn *pgx.Conn, p store.Project) (bool, error) {
	rows, err := conn.Query(ctx, `DELETE FROM xenibase.webhook_outbox RETURNING id`)
	if err != nil {
		return false, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return false, err
	}
	listening := r.s.Listening(p.ID)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxHeld = max(r.maxHeld, r.s.ListenConnections()[r.inst])
	for _, id := range ids {
		r.sent[p.ID] = append(r.sent[p.ID], delivery{id, listening})
	}
	return len(ids) > 0, nil
}

func (r *listenRig) start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	for id := range r.projects {
		r.finished.Add(1)
		go func() { defer r.finished.Done(); r.s.projectLoop(ctx, id) }()
	}
	r.t.Cleanup(func() { cancel(); r.finished.Wait() })
}

// enqueue adds an event to a project's outbox and notifies, as the
// trigger does.
func (r *listenRig) enqueue(id uuid.UUID) {
	r.t.Helper()
	if _, err := r.projects[id].db.Exec(context.Background(),
		`INSERT INTO xenibase.webhook_outbox DEFAULT VALUES; NOTIFY `+OutboxChannel); err != nil {
		r.t.Fatal(err)
	}
}

func (r *listenRig) deliveries(id uuid.UUID) []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery(nil), r.sent[id]...)
}

func (r *listenRig) held() int { return r.s.ListenConnections()[r.inst] }

// open is how many connections the delivery loop has open to a project's
// database, as the server sees them.
func (r *listenRig) open(id uuid.UUID) int {
	r.t.Helper()
	var n int
	if err := r.projects[id].db.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND application_name = 'xenibase-webhooks-test'`).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func eventually(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// With one LISTEN connection allowed per instance and two projects on it,
// one project LISTENs and the other is polled; both get every event, in
// order, and never more than one connection is held.
func TestListenCapPollsTheRest(t *testing.T) {
	r := newListenRig(t, Config{Poll: 100 * time.Millisecond, IdlePoll: 300 * time.Millisecond, IdleAfter: time.Hour, ListenMax: 1}, 2)
	r.start()
	eventually(t, "a LISTEN connection", 10*time.Second, func() bool { return r.held() == 1 })
	var listener, polled uuid.UUID
	for id := range r.projects {
		if r.s.Listening(id) {
			listener = id
		} else {
			polled = id
		}
	}
	if listener == uuid.Nil || polled == uuid.Nil {
		t.Fatal("want one project listening and one polled")
	}
	for range 3 {
		r.enqueue(listener)
		r.enqueue(polled)
	}
	for _, id := range []uuid.UUID{listener, polled} {
		eventually(t, "every event delivered", 10*time.Second, func() bool { return len(r.deliveries(id)) == 3 })
	}
	for id, want := range map[uuid.UUID]bool{listener: true, polled: false} {
		got := r.deliveries(id)
		for i, d := range got {
			if d.listening != want {
				t.Errorf("project listening=%v delivered event %d with listening=%v", want, d.id, d.listening)
			}
			if i > 0 && got[i-1].id >= d.id {
				t.Errorf("out of order: %v", got)
			}
		}
	}
	if r.maxHeld > 1 || r.held() != 1 {
		t.Fatalf("held up to %d LISTEN connections (now %d), want at most 1", r.maxHeld, r.held())
	}
}

// A project whose outbox stays empty gives its LISTEN connection back and
// is polled; events take it again.
func TestIdleProjectReleasesListen(t *testing.T) {
	r := newListenRig(t, Config{Poll: 100 * time.Millisecond, IdlePoll: 200 * time.Millisecond, IdleAfter: 1500 * time.Millisecond, ListenMax: 5}, 1)
	var id uuid.UUID
	for pid := range r.projects {
		id = pid
	}
	r.start()
	eventually(t, "a LISTEN connection", 10*time.Second, func() bool { return r.held() == 1 && r.open(id) == 1 })
	eventually(t, "the idle project to release it", 10*time.Second, func() bool { return r.held() == 0 && !r.s.Listening(id) })
	eventually(t, "its connection closed between polls", 5*time.Second, func() bool { return r.open(id) == 0 })

	// Idle and polled: an event is still delivered, by the poll, and the
	// project takes a LISTEN connection again.
	r.enqueue(id)
	eventually(t, "the event delivered", 10*time.Second, func() bool { return len(r.deliveries(id)) == 1 })
	if r.deliveries(id)[0].listening {
		t.Fatal("the first event after idling was delivered over a LISTEN connection, want by polling")
	}
	eventually(t, "LISTEN taken again", 10*time.Second, func() bool { return r.s.Listening(id) && r.held() == 1 })
	r.enqueue(id)
	eventually(t, "the next event delivered", 10*time.Second, func() bool { return len(r.deliveries(id)) == 2 })
	if !r.deliveries(id)[1].listening {
		t.Fatal("an event after activity resumed was not delivered over the LISTEN connection")
	}
}

// No LISTEN connection at all (a negative cap): projects are polled.
func TestListenOff(t *testing.T) {
	r := newListenRig(t, Config{Poll: 100 * time.Millisecond, IdlePoll: 200 * time.Millisecond, ListenMax: -1}, 1)
	var id uuid.UUID
	for pid := range r.projects {
		id = pid
	}
	r.start()
	r.enqueue(id)
	eventually(t, "the event delivered", 10*time.Second, func() bool { return len(r.deliveries(id)) == 1 })
	if r.maxHeld != 0 || r.held() != 0 {
		t.Fatal("a LISTEN connection was held with the cap off")
	}
}

// The LISTEN connections held per instance go to metric_points, which
// every server's /metrics reads, with a last 0 once an instance has none.
func TestRecordListening(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	s := New(db, nil, nil, nil, nil, nil, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	busy, quiet := uuid.New(), uuid.New()
	for range 2 {
		if !s.takeListen(uuid.New(), busy) {
			t.Fatal("no LISTEN connection to take")
		}
	}
	value := func(inst uuid.UUID) float64 {
		t.Helper()
		var v float64
		if err := db.QueryRow(ctx, `SELECT value FROM metric_points WHERE scope = 'instance' AND scope_id = $1
			AND metric = 'webhook_listen_connections' ORDER BY ts DESC LIMIT 1`, inst).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	seen := map[uuid.UUID]bool{}
	s.recordListening(ctx, []store.Project{{InstanceID: quiet}}, seen)
	if value(busy) != 2 || value(quiet) != 0 {
		t.Fatalf("recorded %v and %v, want 2 and 0", value(busy), value(quiet))
	}
	s.mu.Lock()
	var holders []uuid.UUID
	for p := range s.listening {
		holders = append(holders, p)
	}
	s.mu.Unlock()
	for _, p := range holders {
		s.releaseListen(p)
	}
	s.recordListening(ctx, nil, seen)
	if value(busy) != 0 || len(seen) != 0 {
		t.Fatalf("recorded %v after the connections went (seen %v), want 0", value(busy), seen)
	}
}
