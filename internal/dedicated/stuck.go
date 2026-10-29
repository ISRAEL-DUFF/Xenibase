package dedicated

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Moves, promotions, demotions and major upgrades freeze the source before
// their cutover. A rollback that cannot reach the source (often why the
// operation failed) resumes the poolers but cannot give the logins back,
// so it leaves the project in its moving status, which keeps other
// operations off it, and RepairStuckMoves finishes the rollback once the
// source answers.

// movingStatuses are the statuses a move-like operation runs under.
var movingStatuses = []string{provision.StatusMoving, provision.StatusPromoting, provision.StatusDemoting, provision.StatusUpgrading}

// stuckAfterSeconds is how long after its last operation ended a project
// in a moving status counts as stuck (the operation is marked failed just
// after its rollback returns).
const stuckAfterSeconds = 60

// stuck reports a rollback that could not unfreeze p: the operation fails
// with it, and the sweep retries.
func (s *Service) stuck(ctx context.Context, p store.Project, log *jobs.StepLogger, err error) error {
	_ = log.Error(ctx, "rollback", "the source's logins could not be given back (%v); the project stays %s, and Xenibase retries every minute until the source answers", err, p.Status)
	s.log.Error("rollback left a project frozen; retrying from the sweep", "project_id", p.ID, "status", p.Status, "err", err)
	return fmt.Errorf("unfreeze the source: %w", err)
}

// RepairStuckMoves finishes the rollbacks that could not unfreeze their
// project: for each project in a moving status with no queued or running
// operation, it gives the logins on its current instance LOGIN back,
// resumes the poolers and sets it active. It returns how many it repaired;
// one whose instance still does not answer stays for the next pass.
func (s *Service) RepairStuckMoves(ctx context.Context) (int, error) {
	stuck, err := store.New(s.db).ListStuckProjects(ctx, stuckAfterSeconds)
	if err != nil || len(stuck) == 0 {
		return 0, err
	}
	// The route first: a failed finish may have left it on the old instance.
	if err := s.projects.SyncPooler(ctx, nil, "pooler", "repairing projects a failed move left frozen"); err != nil {
		s.log.Warn("pooler sync before repairing frozen projects", "err", err)
	}
	repaired := 0
	var errs []error
	for _, p := range stuck {
		err := s.projects.WithIdleProject(ctx, p.ID, movingStatuses, func(tx pgx.Tx, cur store.Project) error {
			if err := s.thaw(ctx, cur); err != nil {
				return err
			}
			return store.New(tx).SetProjectStatus(ctx, store.SetProjectStatusParams{ID: cur.ID, Status: provision.StatusActive})
		})
		switch {
		case errors.Is(err, provision.ErrConflict), errors.Is(err, provision.ErrNotFound):
			// An operation took it up, or it was repaired or deleted meanwhile.
		case err != nil:
			s.log.Error("project still frozen after a failed move", "project_id", p.ID, "status", p.Status, "err", err)
			errs = append(errs, fmt.Errorf("project %s: %w", p.ID, err))
		default:
			s.log.Warn("repaired a project a failed move left frozen", "project_id", p.ID, "was", p.Status)
			repaired++
		}
	}
	return repaired, errors.Join(errs...)
}
