package tenancy

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

// maxReapedQuery bounds the query text kept in the reaper's log.
const maxReapedQuery = 1000

// Reaped is one session the reaper acted on.
type Reaped struct {
	ProjectID uuid.UUID
	Kind      string // statement | statement_terminated | idle_in_transaction
	Role      string
	Duration  time.Duration
}

// Reap cancels tenant statements running longer than the hard limit and
// ends sessions idle in a transaction too long, on every shared cluster
// (V2 §10.4): statement_timeout is user-settable, so it cannot be the only
// guard. PL/pgSQL can trap query_canceled, so a statement it already
// cancelled (the same pid and query_start, from reaped_sessions, so any
// server's pass counts) that is still running is ended with its session.
// Dedicated instances are exempt. Durations are measured against the
// service clock.
func (s *Service) Reap(ctx context.Context) ([]Reaped, error) {
	rows, err := store.New(s.db).SharedInstanceProjects(ctx)
	if err != nil {
		return nil, err
	}
	type proj struct{ id, org uuid.UUID }
	byInstance := map[uuid.UUID]map[string]proj{}
	for _, r := range rows {
		if byInstance[r.InstanceID] == nil {
			byInstance[r.InstanceID] = map[string]proj{}
		}
		byInstance[r.InstanceID][r.DbName] = proj{r.ID, r.OrgID}
	}
	now := s.Now()
	var out []Reaped
	var errs []error
	for inst, dbs := range byInstance {
		names := make([]string, 0, len(dbs))
		for n := range dbs {
			names = append(names, n)
		}
		cancelled := func(db string, pid int32, since time.Time) (bool, error) {
			p := dbs[db]
			return store.New(s.db).ReapedStatementCancelled(ctx, store.ReapedStatementCancelledParams{
				ProjectID: p.id, OrgID: p.org, Pid: &pid, StartedAt: &since})
		}
		got, err := s.reapInstance(ctx, inst, names, now, cancelled)
		if err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", inst, err))
		}
		for _, g := range got {
			p := dbs[g.db]
			r := Reaped{ProjectID: p.id, Kind: g.kind, Role: g.role, Duration: g.dur}
			out = append(out, r)
			var query *string
			if g.query != "" {
				query = &g.query
			}
			if err := store.New(s.db).InsertReapedSession(ctx, store.InsertReapedSessionParams{
				ProjectID: p.id, OrgID: p.org, Kind: g.kind, RoleName: g.role, DurationS: int32(g.dur.Seconds()), Query: query,
				Pid: &g.pid, StartedAt: &g.since,
			}); err != nil {
				errs = append(errs, err)
			}
			s.log.Info("reaped", "project_id", p.id, "kind", g.kind, "role", g.role, "duration", g.dur.Round(time.Second))
		}
	}
	return out, errors.Join(errs...)
}

type reapedRow struct {
	db, role, kind, query string
	dur                   time.Duration
	pid                   int32
	since                 time.Time
}

// reapInstance acts on one cluster. cancelled reports whether a statement
// (database, pid, query_start) was cancelled on an earlier pass.
func (s *Service) reapInstance(ctx context.Context, inst uuid.UUID, dbs []string, now time.Time,
	cancelled func(db string, pid int32, since time.Time) (bool, error)) ([]reapedRow, error) {
	conn, err := s.projects.AdminConn(ctx, inst, "postgres")
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `
		SELECT pid, datname, usename, state, COALESCE(left(query, $5), ''),
		       CASE WHEN state = 'active' THEN query_start ELSE xact_start END AS since,
		       CASE WHEN state = 'active' THEN 'statement' ELSE 'idle_in_transaction' END AS kind
		FROM pg_stat_activity
		WHERE backend_type = 'client backend' AND datname = ANY($1) AND usename <> current_user
		  AND pid <> pg_backend_pid()
		  AND ((state = 'active' AND query_start < $2::timestamptz - $3::interval)
		    OR (state IN ('idle in transaction', 'idle in transaction (aborted)') AND state_change < $2::timestamptz - $4::interval))`,
		dbs, now, s.cfg.StatementLimit.String(), s.cfg.IdleTxLimit.String(), maxReapedQuery)
	if err != nil {
		return nil, err
	}
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (reapedRow, error) {
		var c reapedRow
		var state string
		err := r.Scan(&c.pid, &c.db, &c.role, &state, &c.query, &c.since, &c.kind)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	var out []reapedRow
	var errs []error
	for _, c := range cands {
		fn := "pg_cancel_backend"
		if c.kind == "idle_in_transaction" {
			fn = "pg_terminate_backend"
		} else {
			// Cancelled before and still running the same statement: it
			// caught the cancel, so end the session.
			again, err := cancelled(c.db, c.pid, c.since)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if again {
				fn, c.kind = "pg_terminate_backend", "statement_terminated"
			}
		}
		var ok bool
		if err := conn.QueryRow(ctx, "SELECT "+fn+"($1)", c.pid).Scan(&ok); err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			continue // already gone
		}
		c.dur = now.Sub(c.since)
		if !utf8.ValidString(c.query) {
			c.query = ""
		}
		out = append(out, c)
	}
	return out, errors.Join(errs...)
}
