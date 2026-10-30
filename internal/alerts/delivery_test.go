package alerts

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recorder is a webhook receiver that answers 200, or 503 while down.
type recorder struct {
	down atomic.Bool
	mu   sync.Mutex
	got  []Payload
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	var p Payload
	_ = json.NewDecoder(req.Body).Decode(&p)
	r.mu.Lock()
	r.got = append(r.got, p)
	r.mu.Unlock()
}

func (r *recorder) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.got {
		out = append(out, p.Event)
	}
	return out
}

// closedPort is a local TCP port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func exec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// backoffPassed makes every alert and channel due for a retry.
func backoffPassed(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	exec(t, db, `UPDATE alerts SET delivery_lock = now() - interval '1 second' WHERE delivery_lock IS NOT NULL`)
	exec(t, db, `UPDATE alert_deliveries SET next_at = now() - interval '1 second' WHERE next_at IS NOT NULL`)
}

func fire(t *testing.T, db *pgxpool.Pool, key string) uuid.UUID {
	t.Helper()
	row, err := store.New(db).FireAlert(context.Background(), store.FireAlertParams{
		Kind: KindBackupFailed, Key: key, Severity: SeverityCritical, TargetType: "control_plane", TargetID: key,
		TargetName: key, Summary: "test " + key, Detail: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

// A channel that fails is retried alone: the one that took the event is
// not sent it again.
func TestDeliveryRetriesOnlyTheFailedChannel(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	hook := &recorder{}
	srv := httptest.NewServer(hook)
	defer srv.Close()
	s := New(db, nil, Config{}, quiet())
	if _, err := s.Save(ctx, Update{WebhookURL: srv.URL, SMTP: &SMTPUpdate{
		Host: "127.0.0.1", Port: closedPort(t), TLS: TLSNone, From: "x@example.com", To: []string{"ops@example.com"},
	}}); err != nil {
		t.Fatal(err)
	}
	id := fire(t, db, "a")
	for range 3 {
		if err := s.DeliverPending(ctx); err != nil {
			t.Fatal(err)
		}
		backoffPassed(t, db)
	}
	if got := hook.events(); len(got) != 1 || got[0] != EventFiring {
		t.Fatalf("webhook got %v, want one firing whatever email does", got)
	}
	var webhookSent bool
	var emailAttempts int
	if err := db.QueryRow(ctx, `SELECT
		(SELECT sent_at IS NOT NULL FROM alert_deliveries WHERE alert_id = $1 AND channel = 'webhook' AND event = 'firing'),
		(SELECT attempts FROM alert_deliveries WHERE alert_id = $1 AND channel = 'email' AND event = 'firing' AND sent_at IS NULL)`, id).
		Scan(&webhookSent, &emailAttempts); err != nil {
		t.Fatal(err)
	}
	if !webhookSent || emailAttempts != 3 {
		t.Fatalf("webhook sent %v, email attempts %d", webhookSent, emailAttempts)
	}
	var notified *time.Time
	var derr *string
	if err := db.QueryRow(ctx, `SELECT notified_at, delivery_error FROM alerts WHERE id = $1`, id).Scan(&notified, &derr); err != nil {
		t.Fatal(err)
	}
	if notified != nil || derr == nil {
		t.Fatalf("alert marked notified %v with email failing (error %v)", notified, derr)
	}

	// The email channel's own backoff holds it back: no attempt before.
	if err := s.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	exec(t, db, `UPDATE alerts SET delivery_lock = now() - interval '1 second'`)
	if err := s.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT attempts FROM alert_deliveries WHERE alert_id = $1 AND channel = 'email'`, id).Scan(&emailAttempts); err != nil || emailAttempts != 4 {
		t.Fatalf("email attempts %d %v, want 4 (the last one held back by its backoff)", emailAttempts, err)
	}
}

// An alert that resolves before its firing could be delivered is still
// announced, by its resolution.
func TestResolvedBeforeDeliveryIsAnnounced(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	hook := &recorder{}
	hook.down.Store(true)
	srv := httptest.NewServer(hook)
	defer srv.Close()
	s := New(db, nil, Config{}, quiet())
	if _, err := s.Save(ctx, Update{WebhookURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	id := fire(t, db, "short")
	if err := s.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.New(db).ClearAlert(ctx, store.ClearAlertParams{ResolveAfter: 1, ID: id}); err != nil || !ok {
		t.Fatalf("resolve: %v %v", ok, err)
	}
	hook.down.Store(false)
	backoffPassed(t, db)
	if err := s.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := hook.events(); len(got) != 1 || got[0] != EventResolved {
		t.Fatalf("webhook got %v, want the resolution", got)
	}
	var resolvedNotified *time.Time
	if err := db.QueryRow(ctx, `SELECT resolved_notified_at FROM alerts WHERE id = $1`, id).Scan(&resolvedNotified); err != nil || resolvedNotified == nil {
		t.Fatalf("resolved_notified_at %v %v", resolvedNotified, err)
	}
	// Nothing more to send.
	backoffPassed(t, db)
	if err := s.DeliverPending(ctx); err != nil {
		t.Fatal(err)
	}
	if got := hook.events(); len(got) != 1 {
		t.Fatalf("sent again: %v", got)
	}
}

func alertStatus(t *testing.T, db *pgxpool.Pool, key string) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), `SELECT status FROM alerts WHERE key = $1 ORDER BY started_at`, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// A firing alert resolves only after ResolveAfter clear evaluations in a
// row; one that holds again in between stays the same alert.
func TestResolveHysteresis(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	s := New(db, nil, Config{Interval: time.Millisecond}, quiet())
	key := KindBackupFailed + ":control_plane:metadata"
	evaluate := func() {
		t.Helper()
		time.Sleep(2 * time.Millisecond)
		if err := s.Evaluate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	exec(t, db, `INSERT INTO operations (kind, status, error, finished_at) VALUES ('metadata_backup', 'failed', 'x', now())`)
	evaluate()
	exec(t, db, `INSERT INTO operations (kind, status, finished_at) VALUES ('metadata_backup', 'succeeded', now())`)
	evaluate()
	evaluate()
	// It fails again before the third clear evaluation: same alert.
	exec(t, db, `INSERT INTO operations (kind, status, error, finished_at) VALUES ('metadata_backup', 'failed', 'x', now())`)
	evaluate()
	exec(t, db, `INSERT INTO operations (kind, status, finished_at) VALUES ('metadata_backup', 'succeeded', now())`)
	evaluate()
	evaluate()
	if got := alertStatus(t, db, key); len(got) != 1 || got[0] != "firing" {
		t.Fatalf("after two clear evaluations: %v", got)
	}
	evaluate()
	if got := alertStatus(t, db, key); len(got) != 1 || got[0] != "resolved" {
		t.Fatalf("after three clear evaluations: %v", got)
	}
}

// Grace clocks are shared through the database: a second server sees how
// long the first has seen a pooler failing.
func TestGraceSharedAcrossServers(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	ghost, err := pooler.NewAdmin("ghost", "127.0.0.1:1", "xenibase", "x", "disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Poolers: []*pooler.Admin{ghost}, PoolerGrace: 500 * time.Millisecond}
	a, b := New(db, nil, cfg, quiet()), New(db, nil, cfg, quiet())
	if c, err := a.poolerConditions(ctx); err != nil || len(c) != 0 {
		t.Fatalf("down on the first failure: %+v %v", c, err)
	}
	time.Sleep(600 * time.Millisecond)
	if c, err := b.poolerConditions(ctx); err != nil || len(c) != 1 || c[0].kind != KindPoolerDown {
		t.Fatalf("the other server, after the grace period: %+v %v", c, err)
	}
}

// Resolved alerts older than Retain are pruned with their deliveries;
// newer ones and firing ones stay.
func TestPruneResolvedAlerts(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	s := New(db, nil, Config{}, quiet())
	old, recent := fire(t, db, "old"), fire(t, db, "recent")
	fire(t, db, "firing")
	exec(t, db, `UPDATE alerts SET status = 'resolved', resolved_at = now() - interval '91 days', started_at = now() - interval '92 days' WHERE id = $1`, old)
	exec(t, db, `UPDATE alerts SET status = 'resolved', resolved_at = now() - interval '89 days' WHERE id = $1`, recent)
	exec(t, db, `INSERT INTO alert_deliveries (alert_id, event, channel, sent_at) VALUES ($1, 'firing', 'webhook', now())`, old)
	if err := s.prune(ctx); err != nil {
		t.Fatal(err)
	}
	var alerts, deliveries int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM alerts), (SELECT count(*) FROM alert_deliveries)`).Scan(&alerts, &deliveries); err != nil {
		t.Fatal(err)
	}
	if alerts != 2 || deliveries != 0 {
		t.Fatalf("after pruning: %d alerts, %d deliveries", alerts, deliveries)
	}
}

// A failed on-demand restore test of a deleted project does not alert.
func TestRestoreTestOfDeletedProject(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	org, node, inst := uuid.New(), uuid.New(), uuid.New()
	exec(t, db, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'O', 'o', id FROM quota_plans LIMIT 1`, org)
	exec(t, db, `INSERT INTO nodes (id, name, private_addr, role, capacity) VALUES ($1, 'n1', '10.0.0.1', 'both', '{}')`, node)
	exec(t, db, `INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ($1, $2, 'shared', 18, 5432, 'running')`, inst, node)
	project := func(name string, deleted bool) uuid.UUID {
		id := uuid.New()
		exec(t, db, `INSERT INTO projects (id, org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region, deleted_at)
			VALUES ($1, $2, $3, $3, 'p_' || $3, 'p_' || $3 || '_owner', 'x', 'shared', $4, 'active', 'eu-central', CASE WHEN $5 THEN now() END)`,
			id, org, name, inst, deleted)
		exec(t, db, `INSERT INTO operations (kind, project_id, status, error, finished_at) VALUES ('restore_test', $1, 'failed', 'row counts differ', now())`, id)
		return id
	}
	live := project("live", false)
	project("gone", true)
	s := New(db, nil, Config{}, quiet())
	if err := s.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	firing, err := store.New(db).FiringAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range firing {
		if a.Kind == KindRestoreTestFailed {
			got = append(got, a.TargetID)
		}
	}
	if len(got) != 1 || got[0] != live.String() {
		t.Fatalf("restore test alerts for %v, want only the live project %s", got, live)
	}
}

// When one source fails, what the others report still fires, and nothing
// is resolved that round.
func TestFailingSourceFiresTheRest(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	s := New(db, nil, Config{ResolveAfter: 1}, quiet())
	other := fire(t, db, "unrelated")
	exec(t, db, `INSERT INTO operations (kind, status, error, finished_at) VALUES ('metadata_backup', 'failed', 'x', now())`)
	exec(t, db, `ALTER TABLE capacity_proposals RENAME TO capacity_proposals_gone`)
	if err := s.Evaluate(ctx); err == nil {
		t.Fatal("no error with a source failing")
	}
	if got := alertStatus(t, db, KindBackupFailed+":control_plane:metadata"); len(got) != 1 || got[0] != "firing" {
		t.Fatalf("metadata backup alert with another source failing: %v", got)
	}
	var status string
	if err := db.QueryRow(ctx, `SELECT status FROM alerts WHERE id = $1`, other).Scan(&status); err != nil || status != "firing" {
		t.Fatalf("an alert no source reported was resolved during a partial evaluation: %s %v", status, err)
	}
	// Once every source reads again, it resolves.
	exec(t, db, `ALTER TABLE capacity_proposals_gone RENAME TO capacity_proposals`)
	if err := s.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT status FROM alerts WHERE id = $1`, other).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("after a full evaluation: %s %v", status, err)
	}
}
