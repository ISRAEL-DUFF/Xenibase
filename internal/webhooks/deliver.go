package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Delivery rules (V2 §9.1).
const (
	// Timeout bounds one delivery.
	Timeout = 10 * time.Second
	// RetryFor is how long an event is retried before it is dead-lettered.
	RetryFor = 24 * time.Hour
	// PauseAfter consecutive failures pauses a webhook.
	PauseAfter = 50
	// MaxRow is the largest serialised row sent; bigger ones are sent
	// with their primary key only.
	MaxRow = 256 << 10
	// OutboxAlert is the backlog that alerts the project's admins.
	OutboxAlert = 100_000
	// batch is how many events of one webhook one pass sends.
	batch = 100
)

// workerLockKey makes one xenibase-server the deliverer.
const workerLockKey int64 = 0x7067646f636b05 // "xenibase\x05"

// backoff is the delay before attempt n+1 after n failed attempts:
// doubling from 10s to at most 2h, with ±20% jitter, which retries an
// event about 20 times over 24 hours.
func backoff(n int) time.Duration {
	d := 10 * time.Second
	for i := 1; i < n && d < 2*time.Hour; i++ {
		d *= 2
	}
	d = min(d, 2*time.Hour)
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // jitter, not security
}

// Event is the JSON body of a delivery.
type Event struct {
	ID      string `json:"id"`
	Webhook string `json:"webhook"`
	// Project is the project's database name; ProjectID its id in the API.
	Project     string          `json:"project"`
	ProjectID   string          `json:"project_id"`
	Table       string          `json:"table"`
	Type        string          `json:"type"`
	Record      json.RawMessage `json:"record"`
	OldRecord   json.RawMessage `json:"old_record"`
	CommittedAt time.Time       `json:"committed_at"`
	Truncated   bool            `json:"truncated,omitempty"`
	// PrimaryKey is the changed row's primary key, on every change event
	// of a table that has one.
	PrimaryKey map[string]any `json:"primary_key,omitempty"`
}

// EventID is the stable id of an outbox row of webhook id.
func EventID(webhook uuid.UUID, row int64) string {
	return fmt.Sprintf("evt_%s_%d", strings.ReplaceAll(webhook.String(), "-", "")[:12], row)
}

type outboxRow struct {
	ID          int64
	Table, Op   string
	Old, New    []byte
	Replay      []byte
	Attempts    int
	NextAttempt *time.Time
	CreatedAt   time.Time
}

// Run delivers events until ctx ends: one loop per project with webhooks,
// woken by LISTEN and polled every Poll, while this server holds the
// delivery lock.
func (s *Service) Run(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := s.db.Acquire(ctx)
		if err != nil {
			sleep(ctx, 10*time.Second)
			continue
		}
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, workerLockKey).Scan(&got); err != nil || !got {
			conn.Release()
			sleep(ctx, 10*time.Second)
			continue
		}
		s.supervise(ctx)
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, workerLockKey)
		conn.Release()
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// supervise keeps one delivery loop per project with webhooks.
func (s *Service) supervise(ctx context.Context) {
	loops := map[uuid.UUID]context.CancelFunc{}
	defer func() {
		for _, c := range loops {
			c()
		}
	}()
	lastSweep := time.Time{}
	for ctx.Err() == nil {
		projects, err := store.New(s.db).WebhookProjects(ctx)
		if err != nil {
			s.log.Warn("webhook projects", "err", err)
		}
		live := map[uuid.UUID]bool{}
		for _, p := range projects {
			live[p.ID] = true
			if _, ok := loops[p.ID]; !ok {
				lctx, cancel := context.WithCancel(ctx)
				loops[p.ID] = cancel
				go s.projectLoop(lctx, p.ID)
			}
		}
		for id, cancel := range loops {
			if !live[id] {
				cancel()
				delete(loops, id)
			}
		}
		if time.Since(lastSweep) > time.Hour {
			s.sweep(ctx)
			lastSweep = time.Now()
		}
		sleep(ctx, min(s.cfg.Poll, 5*time.Second))
	}
}

func (s *Service) sweep(ctx context.Context) {
	now := s.cfg.Now()
	if _, err := store.New(s.db).SweepDeliveries(ctx, store.SweepDeliveriesParams{LogBefore: now.Add(-7 * 24 * time.Hour), DeadBefore: now.Add(-30 * 24 * time.Hour)}); err != nil && ctx.Err() == nil {
		s.log.Warn("sweep webhook deliveries", "err", err)
	}
	if err := s.out.Sweep(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("sweep outbound counters", "err", err)
	}
}

// projectLoop listens on the project's database and delivers its events,
// reconnecting when the project moves to another instance.
func (s *Service) projectLoop(ctx context.Context, projectID uuid.UUID) {
	kick := make(chan struct{}, 1)
	s.mu.Lock()
	s.kicks[projectID] = kick
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.kicks[projectID] == kick {
			delete(s.kicks, projectID)
		}
		s.mu.Unlock()
	}()
	lastCheck := time.Time{}
	for ctx.Err() == nil {
		p, err := store.New(s.db).GetProject(ctx, projectID)
		if err != nil || p.DeletedAt != nil {
			return
		}
		// Paused while the project moves, restores or resets (V2 §9.3).
		if p.Status != provision.StatusActive {
			sleep(ctx, s.cfg.Poll)
			continue
		}
		conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
		if err != nil {
			sleep(ctx, s.cfg.Poll)
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+OutboxChannel); err != nil {
			_ = conn.Close(context.Background())
			sleep(ctx, s.cfg.Poll)
			continue
		}
		for ctx.Err() == nil {
			cur, err := store.New(s.db).GetProject(ctx, projectID)
			if err != nil || cur.InstanceID != p.InstanceID || cur.Status != provision.StatusActive || cur.DeletedAt != nil {
				break
			}
			if time.Since(lastCheck) > time.Minute {
				s.health(ctx, conn, cur)
				lastCheck = time.Now()
			}
			if err := s.deliverProject(ctx, conn, cur); err != nil && ctx.Err() == nil {
				s.log.Warn("deliver webhooks", "project_id", projectID, "err", err)
				if conn.IsClosed() {
					break
				}
			}
			wctx, cancel := context.WithTimeout(ctx, s.cfg.Poll)
			go func() {
				select {
				case <-kick:
					cancel()
				case <-wctx.Done():
				}
			}()
			_, err = conn.WaitForNotification(wctx)
			cancel()
			if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && conn.IsClosed() {
				break
			}
		}
		_ = conn.Close(context.Background())
	}
}

// health checks triggers and the outbox size.
func (s *Service) health(ctx context.Context, conn *pgx.Conn, p store.Project) {
	hooks, err := store.New(s.db).ListWebhooks(ctx, p.ID)
	if err != nil {
		return
	}
	s.checkTriggers(ctx, conn, hooks)
	if ok, err := outboxExists(ctx, conn); err != nil || !ok {
		return
	}
	var n int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM xenibase.webhook_outbox`).Scan(&n); err != nil {
		return
	}
	s.mu.Lock()
	alerted := s.outboxed[p.ID]
	s.outboxed[p.ID] = n > OutboxAlert
	s.mu.Unlock()
	if n > OutboxAlert && !alerted {
		s.notify(ctx, p, fmt.Sprintf("[Xenibase] %s: %d webhook events are waiting", p.Name, n),
			fmt.Sprintf("%d webhook events of %s are queued, more than %d: a receiver is probably down. They are kept in the project's database until they are delivered, dead-lettered after 24 hours, or the webhook is deleted.", n, p.Name, OutboxAlert))
	}
}

// DeliverNow runs one delivery pass for a project (tests and the API's
// replay use the background loop; this is for callers that want it now).
func (s *Service) DeliverNow(ctx context.Context, p store.Project) error {
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return s.deliverProject(ctx, conn, p)
}

// errQueued stops a pass: the organisation is over its rate or its
// outbound traffic is off, so the events wait.
var errQueued = errors.New("queued")

// deliverProject sends what is due in the project's outbox, in order per
// webhook.
func (s *Service) deliverProject(ctx context.Context, conn *pgx.Conn, p store.Project) error {
	if ok, err := outboxExists(ctx, conn); err != nil || !ok {
		return err
	}
	if err := s.out.Gate(ctx, p.OrgID); err != nil {
		if errors.Is(err, outbound.ErrDisabled) {
			return nil // queued until outbound traffic is back
		}
		return err
	}
	if capped, err := store.New(s.db).OrgSpendCapped(ctx, p.OrgID); err != nil || capped {
		return err // at the spend cap, deliveries queue (V3 §3.10)
	}
	rows, err := conn.Query(ctx, `SELECT DISTINCT webhook_id FROM xenibase.webhook_outbox`)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	hooks, err := store.New(s.db).ListWebhooks(ctx, p.ID)
	if err != nil {
		return err
	}
	byID := map[uuid.UUID]store.Webhook{}
	for _, w := range hooks {
		byID[w.ID] = w
	}
	limit := int64(0)
	if s.limits != nil {
		if l, _, err := s.limits.Limits(ctx, p.OrgID); err == nil {
			limit, _ = l.Get(store.LimitWebhookPerMin)
		}
	}
	for _, id := range ids {
		w, ok := byID[id]
		if !ok {
			// Events of a deleted webhook.
			if _, err := conn.Exec(ctx, `DELETE FROM xenibase.webhook_outbox WHERE webhook_id = $1`, id); err != nil {
				return err
			}
			continue
		}
		if !w.Enabled {
			continue // paused: its queue waits
		}
		if err := s.deliverWebhook(ctx, conn, p, w, limit); err != nil {
			if errors.Is(err, errQueued) {
				return nil
			}
			return err
		}
	}
	return nil
}

func (s *Service) head(ctx context.Context, conn *pgx.Conn, webhook uuid.UUID, n int) ([]outboxRow, error) {
	rows, err := conn.Query(ctx, `SELECT id, table_name, op, old_row, new_row, replay, attempts, next_attempt_at, created_at
		FROM xenibase.webhook_outbox WHERE webhook_id = $1 ORDER BY id LIMIT $2`, webhook, n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (outboxRow, error) {
		var r outboxRow
		return r, row.Scan(&r.ID, &r.Table, &r.Op, &r.Old, &r.New, &r.Replay, &r.Attempts, &r.NextAttempt, &r.CreatedAt)
	})
}

// deliverWebhook sends w's due events in order; a failing event blocks
// the ones behind it until it succeeds or is dead-lettered.
func (s *Service) deliverWebhook(ctx context.Context, conn *pgx.Conn, p store.Project, w store.Webhook, limit int64) error {
	secret, err := s.Secret(w)
	if err != nil {
		return err
	}
	static, err := s.Headers(w)
	if err != nil {
		return err
	}
	previous := s.PreviousSecret(w)
	q := store.New(s.db)
	due, err := s.head(ctx, conn, w.ID, batch)
	if err != nil || len(due) == 0 {
		return err
	}
	// The destination is checked once a batch, and each request goes to
	// the address checked.
	target, perr := s.out.Prepare(ctx, p.OrgID, w.Url)
	if errors.Is(perr, outbound.ErrDisabled) {
		return errQueued
	}
	if perr != nil && !errors.Is(perr, outbound.ErrRefused) {
		return perr
	}
	pks := pkColumns{}
	for _, r := range due {
		now := s.cfg.Now()
		if r.NextAttempt != nil && r.NextAttempt.After(now) {
			return nil // waiting for its retry
		}
		// Over the organisation's rate, events queue (V2 §10.3).
		ok, err := s.out.TakeFrom(p.OrgID, "webhook", limit, time.Minute, func() (int64, error) {
			return q.CountOrgWebhookAttempts(ctx, store.CountOrgWebhookAttemptsParams{OrgID: p.OrgID, Seconds: time.Minute.Seconds()})
		})
		if err != nil {
			return err
		}
		if !ok {
			return errQueued
		}
		eventID, body, err := s.body(ctx, conn, p, w, r, pks)
		if err != nil {
			return err
		}
		h := header(static)
		h.Set("Xenibase-Event-Id", eventID)
		h.Set("Xenibase-Webhook", w.Name)
		resp, derr := outbound.Response{}, perr
		if derr == nil {
			resp, derr = s.out.Send(ctx, target, outbound.Request{OrgID: p.OrgID, URL: w.Url, Header: h, Body: body, Timeout: Timeout,
				Secret: secret, PreviousSecret: previous})
		}
		attempt := r.Attempts + 1
		d := store.InsertDeliveryParams{WebhookID: w.ID, EventID: eventID, Attempt: int32(attempt), CreatedAt: now}
		if resp.Latency > 0 {
			ms := int32(resp.Latency.Milliseconds())
			d.LatencyMs = &ms
		}
		var retry, dead bool
		switch {
		case derr != nil:
			msg := derr.Error()
			d.Error = &msg
			// A destination refused now (its DNS changed) is not retried.
			retry, dead = !errors.Is(derr, outbound.ErrRefused), errors.Is(derr, outbound.ErrRefused)
		default:
			code := int32(resp.StatusCode)
			d.StatusCode = &code
			d.ResponseSnippet = &resp.Snippet
			switch {
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				d.Succeeded = true
			case resp.StatusCode == 429 || resp.StatusCode >= 500:
				retry = true
			default: // other 4xx, and 3xx (redirects are not followed)
				dead = true
			}
		}
		if retry && now.Sub(r.CreatedAt)+backoff(attempt) > RetryFor {
			retry, dead = false, true
		}
		if dead {
			d.DeadLettered = true
			d.Payload = body
		}
		if _, err := q.InsertDelivery(ctx, d); err != nil {
			return err
		}
		if d.Succeeded || dead {
			if _, err := conn.Exec(ctx, `DELETE FROM xenibase.webhook_outbox WHERE id = $1`, r.ID); err != nil {
				return err
			}
		} else {
			msg := ""
			if d.Error != nil {
				msg = *d.Error
			} else if d.StatusCode != nil {
				msg = fmt.Sprintf("HTTP %d", *d.StatusCode)
			}
			if _, err := conn.Exec(ctx, `UPDATE xenibase.webhook_outbox SET attempts = $2, next_attempt_at = $3, last_error = $4 WHERE id = $1`,
				r.ID, attempt, now.Add(backoff(attempt)), msg); err != nil {
				return err
			}
		}
		if w, err = s.recordHealth(ctx, p, w, d.Succeeded); err != nil {
			return err
		}
		if !w.Enabled || retry {
			return nil
		}
	}
	return nil
}

// recordHealth updates w after a delivery; 50 failures in a row pause it
// and tell the project's admins (V2 §9.1).
func (s *Service) recordHealth(ctx context.Context, p store.Project, w store.Webhook, ok bool) (store.Webhook, error) {
	q := store.New(s.db)
	if ok {
		if w.ConsecutiveFailures == 0 && w.Status == StatusHealthy {
			return w, nil
		}
		w.ConsecutiveFailures, w.Status, w.StatusReason = 0, StatusHealthy, nil
		return w, q.SetWebhookHealth(ctx, store.SetWebhookHealthParams{ID: w.ID, Status: w.Status, ConsecutiveFailures: 0, Enabled: w.Enabled})
	}
	w.ConsecutiveFailures++
	if w.Status == StatusHealthy {
		w.Status = StatusFailing
	}
	if w.ConsecutiveFailures >= PauseAfter {
		reason := fmt.Sprintf("paused after %d failed deliveries in a row", w.ConsecutiveFailures)
		w.Enabled, w.Status, w.StatusReason = false, StatusPaused, &reason
		if err := q.SetWebhookHealth(ctx, store.SetWebhookHealthParams{ID: w.ID, Status: w.Status, StatusReason: &reason,
			ConsecutiveFailures: w.ConsecutiveFailures, Enabled: false}); err != nil {
			return w, err
		}
		if err := s.install(ctx, p, w); err != nil { // disables its triggers
			s.log.Warn("disable triggers of a paused webhook", "webhook_id", w.ID, "err", err)
		}
		s.notify(ctx, p, fmt.Sprintf("[Xenibase] Webhook %s of %s was paused", w.Name, p.Name),
			fmt.Sprintf("The webhook %s of %s failed %d deliveries in a row, so Xenibase paused it. Queued events are kept; new changes are not recorded until you resume it. Check the delivery log for the receiver's answers.", w.Name, p.Name, w.ConsecutiveFailures))
		return w, nil
	}
	return w, q.SetWebhookHealth(ctx, store.SetWebhookHealthParams{ID: w.ID, Status: w.Status, StatusReason: w.StatusReason,
		ConsecutiveFailures: w.ConsecutiveFailures, Enabled: w.Enabled})
}

// body is the event's id and JSON for an outbox row.
func (s *Service) body(ctx context.Context, conn *pgx.Conn, p store.Project, w store.Webhook, r outboxRow, pks pkColumns) (string, []byte, error) {
	if len(r.Replay) > 0 {
		var e struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(r.Replay, &e)
		return e.ID, r.Replay, nil
	}
	e := Event{
		ID: EventID(w.ID, r.ID), Webhook: w.Name, Project: store.ClientDBName(p), ProjectID: p.ID.String(), Table: r.Table, Type: r.Op,
		Record: jsonOrNull(r.New), OldRecord: jsonOrNull(r.Old), CommittedAt: r.CreatedAt.UTC(),
		PrimaryKey: primaryKey(ctx, conn, r, pks),
	}
	if len(r.New)+len(r.Old) > MaxRow {
		e.Record, e.OldRecord, e.Truncated = jsonOrNull(nil), jsonOrNull(nil), true
	}
	b, err := json.Marshal(e)
	return e.ID, b, err
}

func jsonOrNull(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

// pkColumns caches tables' primary key columns for a batch.
type pkColumns map[string][]string

func (pks pkColumns) of(ctx context.Context, conn *pgx.Conn, table string) []string {
	if cols, ok := pks[table]; ok {
		return cols
	}
	t := splitTable(table)
	var cols []string
	rows, err := conn.Query(ctx, `SELECT a.attname FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = $1::regclass AND i.indisprimary ORDER BY array_position(i.indkey::int2[], a.attnum)`, t.ident())
	if err == nil {
		cols, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	pks[table] = cols // none (or an error): no primary key in the events
	return cols
}

// primaryKey picks the table's primary key columns out of the row.
func primaryKey(ctx context.Context, conn *pgx.Conn, r outboxRow, pks pkColumns) map[string]any {
	cols := pks.of(ctx, conn, r.Table)
	if len(cols) == 0 {
		return nil
	}
	row := r.New
	if len(row) == 0 {
		row = r.Old
	}
	var all map[string]any
	if json.Unmarshal(row, &all) != nil {
		return nil
	}
	pk := map[string]any{}
	for _, c := range cols {
		pk[c] = all[c]
	}
	return pk
}

// TestResult is the outcome of a test event.
type TestResult struct {
	EventID    string
	StatusCode int
	Latency    time.Duration
	Snippet    string
	Error      string
}

// SendTest posts a test event to w now and records it in the log.
func (s *Service) SendTest(ctx context.Context, p store.Project, w store.Webhook) (TestResult, error) {
	secret, err := s.Secret(w)
	if err != nil {
		return TestResult{}, err
	}
	static, err := s.Headers(w)
	if err != nil {
		return TestResult{}, err
	}
	now := s.cfg.Now()
	id := "evt_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	previous := s.PreviousSecret(w)
	body, _ := json.Marshal(Event{ID: id, Webhook: w.Name, Project: store.ClientDBName(p), ProjectID: p.ID.String(), Type: "TEST",
		Record: json.RawMessage(`{"message":"A test event from Xenibase"}`), OldRecord: json.RawMessage("null"), CommittedAt: now.UTC()})
	h := header(static)
	h.Set("Xenibase-Event-Id", id)
	h.Set("Xenibase-Webhook", w.Name)
	resp, derr := s.out.Do(ctx, outbound.Request{OrgID: p.OrgID, URL: w.Url, Header: h, Body: body, Timeout: Timeout,
		Secret: secret, PreviousSecret: previous})
	res := TestResult{EventID: id, StatusCode: resp.StatusCode, Latency: resp.Latency, Snippet: resp.Snippet}
	d := store.InsertDeliveryParams{WebhookID: w.ID, EventID: id, Attempt: 1, CreatedAt: now}
	if derr != nil {
		res.Error = derr.Error()
		d.Error = &res.Error
	} else {
		code := int32(resp.StatusCode)
		d.StatusCode, d.ResponseSnippet, d.Succeeded = &code, &resp.Snippet, resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	if resp.Latency > 0 {
		ms := int32(resp.Latency.Milliseconds())
		d.LatencyMs = &ms
	}
	if _, err := store.New(s.db).InsertDelivery(ctx, d); err != nil {
		return res, err
	}
	return res, nil
}

// Replay queues dead letters again (ids, or all of w's when nil), after
// the events already waiting. It returns how many were queued.
func (s *Service) Replay(ctx context.Context, p store.Project, w store.Webhook, ids []int64) (int, error) {
	q := store.New(s.db)
	dead, err := q.DeadLetters(ctx, store.DeadLettersParams{WebhookID: w.ID, Ids: ids})
	if err != nil || len(dead) == 0 {
		return 0, err
	}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return 0, err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, outboxDDL); err != nil {
		return 0, err
	}
	done := make([]int64, 0, len(dead))
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		for _, d := range dead {
			var e Event
			_ = json.Unmarshal(d.Payload, &e)
			if _, err := tx.Exec(ctx, `INSERT INTO xenibase.webhook_outbox (webhook_id, table_name, op, replay) VALUES ($1, $2, $3, $4)`,
				w.ID, e.Table, e.Type, d.Payload); err != nil {
				return err
			}
			done = append(done, d.ID)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if err := q.MarkReplayed(ctx, done); err != nil {
		return 0, err
	}
	s.Kick(p.ID)
	return len(done), nil
}
