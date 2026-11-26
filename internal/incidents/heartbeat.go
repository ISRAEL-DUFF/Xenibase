package incidents

import (
	"context"
	"time"

	"github.com/israel-duff/xenibase/internal/alerts"
	"github.com/israel-duff/xenibase/internal/statusapi"
	"github.com/israel-duff/xenibase/internal/store"
)

// edgeSilentAfter: an edge that hasn't reported for this long (it reports
// every 30 seconds at least) counts as stopped.
const edgeSilentAfter = 2 * time.Minute

// SetEdgeSilentAfter changes how long an edge may go without a report
// (tests).
func (s *Service) SetEdgeSilentAfter(d time.Duration) { s.edgeSilent = d }

// States is what xenibase-server knows about the components xenibase-status
// can't probe. That a heartbeat arrives at all also says xenibase-server's
// workers are running.
func (s *Service) States(ctx context.Context) ([]statusapi.ComponentState, error) {
	q := store.New(s.db)
	firing, err := q.FiringAlerts(ctx)
	if err != nil {
		return nil, err
	}
	// The page is public: say what is wrong, not how much or for whom.
	backups := statusapi.ComponentState{ID: ComponentBackups, Status: statusapi.Operational}
	for _, a := range firing {
		switch a.Kind {
		case alerts.KindBackupFailed, alerts.KindBackupOverdue, alerts.KindRestoreTestFailed, alerts.KindWALArchive:
			backups.Status, backups.Detail = statusapi.Degraded, "Some backups are delayed or failing."
		}
	}

	nodes, err := q.DedicatedNodeHealth(ctx)
	if err != nil {
		return nil, err
	}
	dedicated := statusapi.ComponentState{ID: ComponentDedicated, Status: statusapi.Operational}
	switch {
	case nodes.Total > 0 && nodes.Unreachable == nodes.Total:
		dedicated.Status, dedicated.Detail = statusapi.Down, "Dedicated instance hosts are unreachable."
	case nodes.Unreachable > 0:
		dedicated.Status, dedicated.Detail = statusapi.Degraded, "Some dedicated instance hosts are unreachable."
	}

	overdue, err := q.OverdueJobs(ctx)
	if err != nil {
		return nil, err
	}
	automation := statusapi.ComponentState{ID: ComponentWebhooksJobs, Status: statusapi.Operational}
	if overdue > 0 {
		automation.Status, automation.Detail = statusapi.Degraded, "Scheduled jobs are running late."
	}

	states := []statusapi.ComponentState{dedicated, backups, automation}
	if s.Billing != nil {
		b, err := s.Billing(ctx, time.Now())
		if err != nil {
			return nil, err
		}
		states = append(states, b)
	}
	silent := edgeSilentAfter
	if s.edgeSilent > 0 {
		silent = s.edgeSilent
	}
	edges, err := q.EdgeHealth(ctx, time.Now().Add(-silent))
	if err != nil {
		return nil, err
	}
	for _, e := range edges {
		st := statusapi.ComponentState{ID: ComponentBackendServices, Status: statusapi.Operational, Region: e.Region}
		switch {
		case e.Silent == e.Edges:
			st.Status, st.Detail = statusapi.Down, "The backend services gateway isn't responding."
		case e.Silent > 0:
			st.Status, st.Detail = statusapi.Degraded, "Some backend services gateways aren't responding."
		}
		states = append(states, st)
	}
	var out []statusapi.ComponentState
	for _, c := range states {
		for _, id := range s.cfg.Components {
			if id == c.ID {
				out = append(out, c)
			}
		}
	}
	return out, nil
}
