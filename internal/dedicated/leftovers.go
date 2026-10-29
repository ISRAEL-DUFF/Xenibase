package dedicated

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/logical"
	"github.com/israel-duff/xenibase/internal/store"
)

// A move's cleanup is best effort: a side it cannot reach keeps its
// subscription, publication, slot, DDL block or login. A slot with no
// consumer keeps WAL until max_slot_wal_keep_size invalidates it, and the
// DDL block keeps refusing the tenant's schema changes, so a sweep removes
// what no queued or running operation still owns.

// SweepMoveLeftovers removes, on every running instance and shared cluster
// whose node is healthy, the move objects in its projects' databases that
// no queued or running operation owns: subscriptions, publications,
// inactive slots and logins named after a finished operation, and the DDL
// block in a database no such operation is moving. Each object is checked
// against the operations after it is listed, so a move that starts
// meanwhile keeps its own. It returns how many objects it removed.
func (s *Service) SweepMoveLeftovers(ctx context.Context) (int, error) {
	q := store.New(s.db)
	insts, err := q.ListInstanceSummaries(ctx)
	if err != nil {
		return 0, err
	}
	healthy := map[uuid.UUID]bool{}
	removed := 0
	var errs []error
	for _, inst := range insts {
		if inst.Status != "running" {
			continue
		}
		ok, seen := healthy[inst.NodeID]
		if !seen {
			n, err := q.GetNode(ctx, inst.NodeID)
			if err != nil {
				return removed, err
			}
			ok = n.Status == "healthy"
			healthy[inst.NodeID] = ok
		}
		if !ok {
			continue
		}
		n, err := s.sweepInstanceLeftovers(ctx, inst.ID)
		removed += n
		if err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", inst.ID, err))
		}
	}
	return removed, errors.Join(errs...)
}

// sweepInstanceLeftovers is SweepMoveLeftovers on one instance.
func (s *Service) sweepInstanceLeftovers(ctx context.Context, inst uuid.UUID) (int, error) {
	q := store.New(s.db)
	admin, err := s.projects.AdminConn(ctx, inst, "postgres")
	if err != nil {
		return 0, err
	}
	defer admin.Close(context.Background())
	rows, err := admin.Query(ctx, `SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY 1`)
	if err != nil {
		return 0, err
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	// Only Xenibase's own databases (a project's, live, deleted or a
	// retired copy's): anything else on the server is not ours to touch.
	dbs, err := q.ProjectDatabaseNames(ctx, all)
	if err != nil {
		return 0, err
	}
	owned := func(name string) (bool, error) {
		return q.MoveNameInUse(ctx, strings.TrimPrefix(name, logical.Prefix))
	}
	removed := 0
	var errs []error
	logins := map[string]bool{}
	drop := func(what, name string, f func() error) {
		if in, err := owned(name); err != nil || in {
			if err != nil {
				errs = append(errs, err)
			}
			return
		}
		if err := f(); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", what, name, err))
			return
		}
		s.log.Warn("removed a move's leftover replication object", "instance", inst, "kind", what, "name", name)
		removed++
	}
	for _, db := range dbs {
		c, err := s.projects.AdminConn(ctx, inst, db)
		if err != nil {
			errs = append(errs, fmt.Errorf("database %s: %w", db, err))
			continue
		}
		lo, err := logical.FindLeftovers(ctx, c)
		if err != nil {
			errs = append(errs, fmt.Errorf("database %s: %w", db, err))
			_ = c.Close(context.Background())
			continue
		}
		for _, n := range lo.Subscriptions {
			drop("subscription", n, func() error { return logical.DropSubscription(ctx, c, n) })
		}
		for _, n := range lo.Slots {
			drop("slot", n, func() error { return logical.DropInactiveSlot(ctx, c, n) })
		}
		for _, n := range lo.Publications {
			drop("publication", n, func() error { return logical.DropPublication(ctx, c, n) })
		}
		for _, n := range lo.Logins {
			if in, err := owned(n); err != nil || in {
				continue
			}
			if err := logical.DropLoginGrants(ctx, c, n); err != nil {
				errs = append(errs, fmt.Errorf("login %s: %w", n, err))
				continue
			}
			logins[n] = true
		}
		if lo.DDLBlock {
			busy, err := q.MoveDatabaseBusy(ctx, store.MoveDatabaseBusyParams{DbName: db, InstanceID: inst.String()})
			switch {
			case err != nil:
				errs = append(errs, err)
			case !busy:
				if err := logical.DropDDLBlock(ctx, c); err != nil {
					errs = append(errs, fmt.Errorf("database %s: %w", db, err))
				} else {
					s.log.Warn("removed a move's leftover DDL block", "instance", inst, "database", db)
					removed++
				}
			}
		}
		_ = c.Close(context.Background())
	}
	for n := range logins {
		drop("login", n, func() error { return logical.DropLogin(ctx, admin, n) })
	}
	return removed, errors.Join(errs...)
}

// RunMoveSweep calls SweepMoveLeftovers every interval until ctx ends.
func (s *Service) RunMoveSweep(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.SweepMoveLeftovers(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("move leftover sweep", "err", err)
		}
	}
}
