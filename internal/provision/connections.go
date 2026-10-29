package provision

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
)

// A dedicated instance's max_connections grows with its memory: 50 per GB,
// never below 100 (what every size had before) nor above 500. Each
// connection is a backend process, and 50 per GB leaves room for their
// work_mem next to shared_buffers (a quarter of memory).
const (
	MinMaxConnections = 100
	MaxMaxConnections = 500
	connectionsPerGB  = 50
	// ConnectionReserve is what a dedicated project's own logins leave of
	// max_connections: Xenibase's (backups, HA replication, the SLA probe,
	// backend services, admin operations) and the superuser slots.
	ConnectionReserve = 10
)

// MaxConnectionsFor is the max_connections of a dedicated instance with
// memMB of memory.
func MaxConnectionsFor(memMB int) int {
	return min(max(memMB/1024*connectionsPerGB, MinMaxConnections), MaxMaxConnections)
}

// ConnectionLimitMax is the highest connection limit a dedicated project
// on an instance with maxConnections may have.
func ConnectionLimitMax(maxConnections int32) int {
	return max(int(maxConnections)-ConnectionReserve, 1)
}

// dedicatedLimitMax is the connection limit p's instance allows, and false
// for a shared project (its cluster's limits are the plan's).
func (s *Service) dedicatedLimitMax(ctx context.Context, q *store.Queries, p store.Project) (int, bool, error) {
	if p.Tier != TierDedicated {
		return 0, false, nil
	}
	inst, err := q.GetInstance(ctx, p.InstanceID)
	if err != nil {
		return 0, false, err
	}
	return ConnectionLimitMax(inst.MaxConnections), true, nil
}

// checkConnectionLimit refuses a dedicated project's connection limit its
// instance can't hold.
func (s *Service) checkConnectionLimit(ctx context.Context, q *store.Queries, p store.Project, limit int) error {
	most, ok, err := s.dedicatedLimitMax(ctx, q, p)
	if err != nil || !ok || limit <= most {
		return err
	}
	return fmt.Errorf("%w: connection_limit can be at most %d on this instance (max_connections %d, %d kept for Xenibase); a larger instance allows more",
		ErrInvalid, most, most+ConnectionReserve, ConnectionReserve)
}

// FitConnectionLimit lowers a dedicated project's connection limit (and
// pool size) to what its instance now allows, after a resize to less
// memory, and applies it to the role and the poolers. The caller's
// operation owns the project. It reports the limit it set, or 0 when
// nothing changed.
func (s *Service) FitConnectionLimit(ctx context.Context, projectID uuid.UUID, log *jobs.StepLogger) (int, error) {
	q := store.New(s.db)
	p, err := q.GetProject(ctx, projectID)
	if err != nil {
		return 0, err
	}
	most, ok, err := s.dedicatedLimitMax(ctx, q, p)
	if err != nil || !ok {
		return 0, err
	}
	set, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		return 0, err
	}
	if set.ConnectionLimit <= most {
		return 0, nil
	}
	set.ConnectionLimit, set.PoolSize = most, min(set.PoolSize, most)
	raw, err := json.Marshal(set)
	if err != nil {
		return 0, err
	}
	if p, err = q.UpdateProjectMeta(ctx, store.UpdateProjectMetaParams{ID: p.ID, Name: p.Name, Description: p.Description, Settings: raw}); err != nil {
		return 0, err
	}
	return most, s.applySettings(ctx, p, set, log)
}
