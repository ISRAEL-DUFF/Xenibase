package statuspage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite: the status service has no C dependencies

	"github.com/israel-duff/xenibase/internal/statusapi"
)

// schema is the whole state. Times are Unix seconds (UTC). States in
// minutes are statusapi.Rank values so "worst in the minute" is max().
const schema = `
CREATE TABLE IF NOT EXISTS minutes (
  component TEXT NOT NULL, minute INTEGER NOT NULL, state INTEGER NOT NULL,
  PRIMARY KEY (component, minute)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS days (
  component TEXT NOT NULL, day TEXT NOT NULL,
  up INTEGER NOT NULL, degraded INTEGER NOT NULL, down INTEGER NOT NULL, unknown INTEGER NOT NULL,
  PRIMARY KEY (component, day)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS current (
  component TEXT PRIMARY KEY, state TEXT NOT NULL, detail TEXT NOT NULL,
  since INTEGER NOT NULL, checked_at INTEGER NOT NULL,
  fails INTEGER NOT NULL, oks INTEGER NOT NULL, auto_incident TEXT
);
CREATE TABLE IF NOT EXISTS heartbeats (
  component TEXT PRIMARY KEY, state TEXT NOT NULL, detail TEXT NOT NULL, received_at INTEGER NOT NULL
);
-- The sent_at (Unix milliseconds) of the newest heartbeat accepted: an
-- older one (a replay) is refused.
CREATE TABLE IF NOT EXISTS heartbeat_latest (
  id INTEGER PRIMARY KEY CHECK (id = 1), sent_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS incidents (
  id TEXT PRIMARY KEY, title TEXT NOT NULL, components TEXT NOT NULL, region TEXT NOT NULL,
  severity TEXT NOT NULL, status TEXT NOT NULL, auto INTEGER NOT NULL,
  started_at INTEGER NOT NULL, resolved_at INTEGER, updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS incidents_started ON incidents (started_at);
CREATE TABLE IF NOT EXISTS incident_updates (
  incident_id TEXT NOT NULL, id TEXT NOT NULL, status TEXT NOT NULL, body TEXT NOT NULL, posted_at INTEGER NOT NULL,
  PRIMARY KEY (incident_id, id)
);
CREATE TABLE IF NOT EXISTS subscribers (
  email TEXT PRIMARY KEY, confirmed_at INTEGER, confirm_hash TEXT, confirm_sent_at INTEGER,
  unsub_token TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS subscribers_confirm ON subscribers (confirm_hash);
CREATE TABLE IF NOT EXISTS managed_subscribers (
  email TEXT PRIMARY KEY, components TEXT NOT NULL, regions TEXT NOT NULL,
  unsub_token TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS unsubscribed (
  email TEXT PRIMARY KEY, at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT, recipient TEXT NOT NULL, subject TEXT NOT NULL, body TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL, last_error TEXT
);
`

type store struct{ db *sql.DB }

func openStore(path string) (*store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// One writer at a time is all SQLite does; one connection avoids
	// "database is locked" between goroutines.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + slaSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("status database %s: %w", path, err)
	}
	return &store{db: db}, nil
}

func (s *store) Close() error { return s.db.Close() }

// track is a component's running state.
type track struct {
	State        string
	Detail       string
	Since        time.Time
	CheckedAt    time.Time
	Fails, Oks   int
	AutoIncident string
}

func (s *store) tracks(ctx context.Context) (map[string]track, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT component, state, detail, since, checked_at, fails, oks, auto_incident FROM current`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]track{}
	for rows.Next() {
		var id string
		var t track
		var since, checked int64
		var auto sql.NullString
		if err := rows.Scan(&id, &t.State, &t.Detail, &since, &checked, &t.Fails, &t.Oks, &auto); err != nil {
			return nil, err
		}
		t.Since, t.CheckedAt, t.AutoIncident = time.Unix(since, 0).UTC(), time.Unix(checked, 0).UTC(), auto.String
		out[id] = t
	}
	return out, rows.Err()
}

func (s *store) saveTrack(ctx context.Context, tx *sql.Tx, id string, t track) error {
	var auto any
	if t.AutoIncident != "" {
		auto = t.AutoIncident
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO current (component, state, detail, since, checked_at, fails, oks, auto_incident)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (component) DO UPDATE SET state = excluded.state, detail = excluded.detail, since = excluded.since,
  checked_at = excluded.checked_at, fails = excluded.fails, oks = excluded.oks, auto_incident = excluded.auto_incident`,
		id, t.State, t.Detail, t.Since.Unix(), t.CheckedAt.Unix(), t.Fails, t.Oks, auto)
	return err
}

// recordMinute keeps the worst state seen in the minute.
func recordMinute(ctx context.Context, tx *sql.Tx, id string, minute time.Time, state string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO minutes (component, minute, state) VALUES (?, ?, ?)
ON CONFLICT (component, minute) DO UPDATE SET state = max(state, excluded.state)`, id, minute.Unix(), statusapi.Rank(state))
	return err
}

// rollUp recomputes the daily counts from from's day on, and drops minutes
// older than minuteKeep and days older than dayKeep.
func (s *store) rollUp(ctx context.Context, from time.Time, now time.Time) error {
	day := from.UTC().Truncate(24 * time.Hour)
	_, err := s.db.ExecContext(ctx, `INSERT INTO days (component, day, up, degraded, down, unknown)
SELECT component, date(minute, 'unixepoch'), sum(state = 0), sum(state = 2), sum(state = 3), sum(state = 1)
FROM minutes WHERE minute >= ? GROUP BY 1, 2
ON CONFLICT (component, day) DO UPDATE SET up = excluded.up, degraded = excluded.degraded,
  down = excluded.down, unknown = excluded.unknown`, day.Unix())
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM minutes WHERE minute < ?`, now.Add(-minuteKeep).Unix()); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM days WHERE day < ?`, now.UTC().Add(-dayKeep).Format(time.DateOnly))
	return err
}

const (
	minuteKeep = 3 * 24 * time.Hour
	dayKeep    = 90 * 24 * time.Hour
)

// dayCount is one component's minutes in one UTC day.
type dayCount struct {
	Day                         string
	Up, Degraded, Down, Unknown int
}

func (s *store) days(ctx context.Context, since time.Time) (map[string]map[string]dayCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT component, day, up, degraded, down, unknown FROM days WHERE day >= ?`, since.UTC().Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]dayCount{}
	for rows.Next() {
		var id string
		var d dayCount
		if err := rows.Scan(&id, &d.Day, &d.Up, &d.Degraded, &d.Down, &d.Unknown); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]dayCount{}
		}
		out[id][d.Day] = d
	}
	return out, rows.Err()
}

func (s *store) heartbeats(ctx context.Context) (map[string]heartbeat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT component, state, detail, received_at FROM heartbeats`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]heartbeat{}
	for rows.Next() {
		var id string
		var h heartbeat
		var at int64
		if err := rows.Scan(&id, &h.State, &h.Detail, &at); err != nil {
			return nil, err
		}
		h.At = time.Unix(at, 0).UTC()
		out[id] = h
	}
	return out, rows.Err()
}

type heartbeat struct {
	State, Detail string
	At            time.Time
}

// errStaleHeartbeat: a heartbeat sent before the newest one accepted (a
// replay, or one overtaken in transit) would overwrite newer states.
var errStaleHeartbeat = errors.New("a newer heartbeat has already been accepted")

func (s *store) saveHeartbeat(ctx context.Context, states map[string]statusapi.ComponentState, sentAt, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var latest int64
	switch err := tx.QueryRowContext(ctx, `SELECT sent_at FROM heartbeat_latest WHERE id = 1`).Scan(&latest); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case sentAt.UnixMilli() < latest:
		return errStaleHeartbeat
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO heartbeat_latest (id, sent_at) VALUES (1, ?)
ON CONFLICT (id) DO UPDATE SET sent_at = max(sent_at, excluded.sent_at)`, sentAt.UnixMilli()); err != nil {
		return err
	}
	for id, c := range states {
		if _, err := tx.ExecContext(ctx, `INSERT INTO heartbeats (component, state, detail, received_at) VALUES (?, ?, ?, ?)
ON CONFLICT (component) DO UPDATE SET state = excluded.state, detail = excluded.detail, received_at = excluded.received_at`,
			id, c.Status, c.Detail, now.Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Incidents.

// querier is the database or a transaction: with one connection, reads
// inside a transaction must go through it.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *store) incident(ctx context.Context, q querier, id string) (*statusapi.Incident, error) {
	list, err := s.queryIncidents(ctx, q, `WHERE id = ?`, id)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// incidentsSince lists incidents that started after since or are still
// open, newest first.
func (s *store) incidentsSince(ctx context.Context, since time.Time) ([]statusapi.Incident, error) {
	return s.queryIncidents(ctx, s.db, `WHERE started_at >= ? OR resolved_at IS NULL ORDER BY started_at DESC`, since.Unix())
}

func (s *store) queryIncidents(ctx context.Context, q querier, where string, args ...any) ([]statusapi.Incident, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, title, components, region, severity, status, auto, started_at, resolved_at FROM incidents `+where, args...)
	if err != nil {
		return nil, err
	}
	var out []statusapi.Incident
	for rows.Next() {
		var in statusapi.Incident
		var comps string
		var started int64
		var resolved sql.NullInt64
		if err := rows.Scan(&in.ID, &in.Title, &comps, &in.Region, &in.Severity, &in.Status, &in.Auto, &started, &resolved); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(comps), &in.Components)
		in.StartedAt = time.Unix(started, 0).UTC()
		if resolved.Valid {
			t := time.Unix(resolved.Int64, 0).UTC()
			in.ResolvedAt = &t
		}
		out = append(out, in)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		ups, err := s.updates(ctx, q, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Updates = ups
	}
	return out, nil
}

func (s *store) updates(ctx context.Context, q querier, incident string) ([]statusapi.IncidentUpdate, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, status, body, posted_at FROM incident_updates WHERE incident_id = ? ORDER BY posted_at, id`, incident)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []statusapi.IncidentUpdate
	for rows.Next() {
		var u statusapi.IncidentUpdate
		var at int64
		if err := rows.Scan(&u.ID, &u.Status, &u.Body, &at); err != nil {
			return nil, err
		}
		u.PostedAt = time.Unix(at, 0).UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

// errAutoIncident: xenibase-server may not overwrite an incident the status
// service opened itself.
var errAutoIncident = errors.New("this incident was opened by the status service and is managed by it")

// putIncident stores in (replacing its updates) and returns the updates
// that are new, for notifications.
func (s *store) putIncident(ctx context.Context, tx *sql.Tx, in statusapi.Incident, now time.Time) ([]statusapi.IncidentUpdate, error) {
	var auto bool
	err := tx.QueryRowContext(ctx, `SELECT auto FROM incidents WHERE id = ?`, in.ID).Scan(&auto)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	case auto && !in.Auto:
		return nil, errAutoIncident
	}
	old := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM incident_updates WHERE incident_id = ?`, in.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		old[id] = true
	}
	_ = rows.Close()
	comps, _ := json.Marshal(in.Components)
	var resolved any
	if in.ResolvedAt != nil {
		resolved = in.ResolvedAt.Unix()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO incidents (id, title, components, region, severity, status, auto, started_at, resolved_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET title = excluded.title, components = excluded.components, region = excluded.region,
  severity = excluded.severity, status = excluded.status, started_at = excluded.started_at,
  resolved_at = excluded.resolved_at, updated_at = excluded.updated_at`,
		in.ID, in.Title, string(comps), in.Region, in.Severity, in.Status, in.Auto, in.StartedAt.Unix(), resolved, now.Unix()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM incident_updates WHERE incident_id = ?`, in.ID); err != nil {
		return nil, err
	}
	var fresh []statusapi.IncidentUpdate
	for _, u := range in.Updates {
		if _, err := tx.ExecContext(ctx, `INSERT INTO incident_updates (incident_id, id, status, body, posted_at) VALUES (?, ?, ?, ?, ?)`,
			in.ID, u.ID, u.Status, u.Body, u.PostedAt.Unix()); err != nil {
			return nil, err
		}
		if !old[u.ID] {
			fresh = append(fresh, u)
		}
	}
	return fresh, nil
}
