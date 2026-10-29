package provision

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

// OrphanGrace is how long a deleted project's database and roles may stay
// on a shared cluster before the sweep drops them: the delete and the
// rollback's own cleanup have had their chance by then.
const OrphanGrace = 24 * time.Hour

// projectPrefixRe matches an opaque project's database, and the start of
// each of its roles' names.
var projectPrefixRe = regexp.MustCompile(`^p_[a-z2-7]{10}`)

// OrphanReport is what one sweep found on the shared clusters.
type OrphanReport struct {
	// Dropped are databases (by name) whose project was deleted more than
	// the grace period ago; their database and roles were dropped.
	Dropped []string
	// Unknown are names no project in the metadata has ever used. They are
	// reported, never dropped: they may belong to another installation.
	Unknown []string
}

// SweepOrphans compares each running shared cluster's opaque databases
// and roles (p_… names) with the projects in the metadata, and drops those
// of a project deleted more than grace ago on that cluster with no
// operation pending. A name that is live anywhere, busy, recently deleted,
// or unknown is left alone.
func (s *Service) SweepOrphans(ctx context.Context, grace time.Duration) (OrphanReport, error) {
	var rep OrphanReport
	q := store.New(s.db)
	insts, err := q.ListLiveSharedInstances(ctx)
	if err != nil {
		return rep, err
	}
	for _, inst := range insts {
		if inst.Status != "running" {
			continue
		}
		dropped, unknown, err := s.sweepInstance(ctx, inst.ID, grace)
		if err != nil {
			s.log.Warn("orphan sweep: shared cluster skipped", "instance", inst.ID, "node", inst.NodeName, "err", err)
			continue
		}
		rep.Dropped = append(rep.Dropped, dropped...)
		rep.Unknown = append(rep.Unknown, unknown...)
	}
	return rep, nil
}

func (s *Service) sweepInstance(ctx context.Context, instanceID uuid.UUID, grace time.Duration) (dropped, unknown []string, err error) {
	conn, err := s.connectInstance(ctx, instanceID, "postgres")
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close(context.Background())

	rows, err := conn.Query(ctx, `
		SELECT datname FROM pg_database WHERE datname ~ '^p_[a-z2-7]{10}$'
		UNION SELECT rolname FROM pg_roles WHERE rolname ~ '^p_[a-z2-7]{10}_'`)
	if err != nil {
		return nil, nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, nil, err
	}
	var prefixes []string
	for _, name := range found {
		if p := projectPrefixRe.FindString(name); p != "" && !slices.Contains(prefixes, p) {
			prefixes = append(prefixes, p)
		}
	}
	if len(prefixes) == 0 {
		return nil, nil, nil
	}
	owners, err := store.New(s.db).ProjectsByDBName(ctx, prefixes)
	if err != nil {
		return nil, nil, err
	}
	cutoff := time.Now().Add(-grace)
	for _, db := range prefixes {
		known, keep, here := false, false, false
		for _, o := range owners {
			if o.DbName != db {
				continue
			}
			known = true
			// Live, busy, or recently deleted anywhere: not an orphan (yet).
			keep = keep || o.DeletedAt == nil || o.Busy || o.DeletedAt.After(cutoff)
			here = here || (o.InstanceID == instanceID && o.Tier == TierShared)
		}
		switch {
		case !known:
			unknown = append(unknown, db)
			s.log.Warn("orphan sweep: a project database or role no project uses; not dropped", "instance", instanceID, "name", db)
		case keep || !here:
		default:
			if err := dropProjectPrefix(ctx, conn, db); err != nil {
				s.log.Warn("orphan sweep: could not drop a deleted project's objects", "instance", instanceID, "database", db, "err", err)
				continue
			}
			s.log.Info("orphan sweep: dropped a deleted project's database and roles", "instance", instanceID, "database", db)
			dropped = append(dropped, db)
		}
	}
	return dropped, unknown, nil
}

// dropProjectPrefix drops database db and every role named db_….
func dropProjectPrefix(ctx context.Context, conn *pgx.Conn, db string) error {
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident(db)+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database: %w", err)
	}
	rows, err := conn.Query(ctx, `SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1) ORDER BY rolname = $2`,
		db+"_", OwnerRoleName(db))
	if err != nil {
		return err
	}
	roles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// The owner goes last: the others may be members of it.
	for _, r := range roles {
		if r == ConsoleRole(db) {
			if err := DropConsoleRole(ctx, conn, r, db); err != nil {
				return fmt.Errorf("drop role %s: %w", r, err)
			}
			continue
		}
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+ident(r)); err != nil {
			return fmt.Errorf("drop role %s: %w", r, err)
		}
	}
	return nil
}

// RunOrphanSweep calls SweepOrphans every interval until ctx ends.
func (s *Service) RunOrphanSweep(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.SweepOrphans(ctx, OrphanGrace); err != nil && ctx.Err() == nil {
			s.log.Warn("orphan sweep failed", "err", err)
		}
	}
}
