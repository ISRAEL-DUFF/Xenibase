package capacity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/cloud"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Move kinds and statuses (rebalance_moves).
const (
	MoveDrain     = "drain"
	MoveRebalance = "rebalance"

	MoveProposed = "proposed"
	MoveApproved = "approved"
	MoveMoving   = "moving"
	MoveDone     = "done"
	MoveFailed   = "failed"
	MoveSkipped  = "skipped"
	MoveRejected = "rejected"
)

// Drain moves every project off a node, one at a time with zero-downtime
// moves (V3 §5.3); nothing new is placed on it meanwhile. HA projects
// move by switchover, not by drain: the drain skips them and says so.
func (s *Service) Drain(ctx context.Context, nodeID uuid.UUID) (store.Node, int, error) {
	q := store.New(s.db)
	n, err := q.GetNode(ctx, nodeID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && n.Status == "removed") {
		return n, 0, ErrNotFound
	}
	if err != nil {
		return n, 0, err
	}
	if n.Role == "pooler" {
		return n, 0, invalid("a pooler host has no projects to drain")
	}
	if n, err = q.SetNodeLifecycle(ctx, store.SetNodeLifecycleParams{ID: nodeID, Lifecycle: "draining"}); err != nil {
		return n, 0, err
	}
	ps, err := q.NodeProjects(ctx, nodeID)
	if err != nil {
		return n, 0, err
	}
	batch := uuid.New()
	queued := 0
	for _, p := range ps {
		_, err := q.InsertRebalanceMove(ctx, store.InsertRebalanceMoveParams{
			Batch: batch, Kind: MoveDrain, ProjectID: p.ID, FromNode: nodeID, Reason: "draining " + n.Name, Status: MoveApproved,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // a move is already open for it
		}
		if err != nil {
			return n, queued, err
		}
		queued++
	}
	s.log.Info("draining node", "node", n.Name, "projects", queued)
	return n, queued, nil
}

// StopDrain puts a draining node back into service; moves not started
// are dropped.
func (s *Service) StopDrain(ctx context.Context, nodeID uuid.UUID) (store.Node, error) {
	q := store.New(s.db)
	n, err := q.SetNodeLifecycle(ctx, store.SetNodeLifecycleParams{ID: nodeID, Lifecycle: "active"})
	if errors.Is(err, pgx.ErrNoRows) {
		return n, ErrNotFound
	}
	if err != nil {
		return n, err
	}
	_, err = q.CancelNodeDrain(ctx, nodeID)
	return n, err
}

// target picks where a project on from goes: the active, healthy node in
// the same region with the most room that can take it.
func (s *Service) target(ctx context.Context, p store.Project, from store.Node) (uuid.UUID, error) {
	q := store.New(s.db)
	ns, err := q.CapacityNodes(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	src, err := q.GetInstance(ctx, p.InstanceID)
	if err != nil {
		return uuid.Nil, err
	}
	best, bestScore := uuid.Nil, math.Inf(-1)
	for _, n := range ns {
		if n.ID == from.ID || n.Region != from.Region || n.Lifecycle != "active" || n.Status != "healthy" || n.AgentCertFp == nil {
			continue
		}
		m := hostMetrics(n.Capacity)
		if p.Tier == provision.TierShared {
			if n.Role != "shared" && n.Role != "both" {
				continue
			}
			c, err := q.SharedInstanceOnNode(ctx, n.ID)
			if err != nil || c.Status != "running" || c.PgVersion != src.PgVersion || (c.OrgID != nil && *c.OrgID != p.OrgID) || (src.OrgID != nil && c.OrgID == nil) {
				continue
			}
			if m.DiskTotalBytes <= 0 {
				continue
			}
			if score := float64(m.DiskFreeBytes) / float64(m.DiskTotalBytes); score > bestScore {
				best, bestScore = n.ID, score
			}
			continue
		}
		if n.Role != "dedicated" && n.Role != "both" {
			continue
		}
		prof, ok := dedicated.ProfileByName(deref(src.Profile))
		if !ok {
			prof = dedicated.Profiles[0]
		}
		freeCPU := float64(m.CPUs) - n.DedicatedCpus
		freeMem := m.MemTotalBytes/(1<<20) - n.ReservedMemMb
		if freeCPU < prof.CPUs || freeMem < int64(prof.MemoryMB) {
			continue
		}
		if score := freeCPU; score > bestScore {
			best, bestScore = n.ID, score
		}
	}
	if best == uuid.Nil {
		return uuid.Nil, fmt.Errorf("no node in %s can take it (an active, healthy %s node with room and Postgres %d)", from.Region, p.Tier, src.PgVersion)
	}
	return best, nil
}

// advanceMoves finishes moves whose operation ended and starts the next
// approved one: one move at a time across the platform, as drains are for
// maintenance and nothing is in a hurry.
func (s *Service) advanceMoves(ctx context.Context) error {
	q := store.New(s.db)
	moving, err := q.MovingRebalanceMoves(ctx)
	if err != nil {
		return err
	}
	busy := false
	for _, m := range moving {
		if m.OperationID == nil {
			continue
		}
		op, err := jobs.Get(ctx, s.db, *m.OperationID)
		if err != nil {
			return err
		}
		if !jobs.IsTerminal(op.Status) {
			busy = true
			continue
		}
		status, msg := MoveDone, (*string)(nil)
		if op.Status != "succeeded" {
			status, msg = MoveFailed, op.Error
		}
		if err := q.SetRebalanceMove(ctx, store.SetRebalanceMoveParams{ID: m.ID, Status: status, Error: msg}); err != nil {
			return err
		}
	}
	if busy {
		return nil
	}
	if err := s.autoApprove(ctx); err != nil {
		return err
	}
	// No transaction is held across the move: Move takes connections of
	// its own, and only this loop starts moves.
	m, err := q.NextApprovedMove(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// With automatic rebalancing, rebalance moves run only inside the
	// maintenance window (V4.1 §8.4); drains don't wait.
	if m.Kind == MoveRebalance {
		if st, err := s.Settings(ctx); err != nil {
			return err
		} else if st.AutoRebalance {
			w, err := s.ded.MaintenanceWindow(ctx)
			if err != nil {
				return err
			}
			if !w.Contains(s.cfg.Now()) {
				return nil
			}
		}
	}
	fail := func(status, msg string) error {
		return q.SetRebalanceMove(ctx, store.SetRebalanceMoveParams{ID: m.ID, Status: status, Error: &msg})
	}
	p, err := q.GetProject(ctx, m.ProjectID)
	if err != nil || p.DeletedAt != nil {
		return fail(MoveSkipped, "the project was deleted")
	}
	inst, err := q.GetInstance(ctx, p.InstanceID)
	if err != nil {
		return err
	}
	if inst.NodeID != m.FromNode {
		return fail(MoveSkipped, "the project is no longer on that node")
	}
	if inst.HaEnabled {
		return fail(MoveSkipped, "HA project: switch its primary over, or turn HA off, to move it")
	}
	switch p.Lifecycle {
	case "paused":
		return fail(MoveSkipped, "a paused Free project: resume it, then drain again")
	case "archived":
		return fail(MoveSkipped, "an archived Free project: its roles live on this node's cluster; unarchive it, then drain again")
	}
	from, err := q.GetNode(ctx, m.FromNode)
	if err != nil {
		return err
	}
	var to uuid.UUID
	if m.ToNode != nil {
		to = *m.ToNode
	} else if to, err = s.target(ctx, p, from); err != nil {
		return fail(MoveFailed, err.Error())
	}
	op, err := s.ded.Move(ctx, dedicated.MoveParams{ProjectID: p.ID, NodeID: to})
	if err != nil {
		if errors.Is(err, provision.ErrConflict) && p.Status != provision.StatusActive {
			return nil // busy with another operation: try again on the next step
		}
		return fail(MoveFailed, err.Error())
	}
	s.log.Info("moving project", "project", p.ID, "from", from.Name, "kind", m.Kind, "operation", op.ID)
	return q.SetRebalanceMove(ctx, store.SetRebalanceMoveParams{ID: m.ID, Status: MoveMoving, ToNode: &to, OperationID: &op.ID})
}

// autoApprove approves rebalance batches in the maintenance window when
// automatic rebalancing is on.
func (s *Service) autoApprove(ctx context.Context) error {
	st, err := s.Settings(ctx)
	if err != nil || !st.AutoRebalance {
		return err
	}
	w, err := s.ded.MaintenanceWindow(ctx)
	if err != nil || !w.Contains(s.cfg.Now()) {
		return err
	}
	ms, err := store.New(s.db).ListRebalanceMoves(ctx)
	if err != nil {
		return err
	}
	seen := map[uuid.UUID]bool{}
	for _, m := range ms {
		if m.Kind == MoveRebalance && m.Status == MoveProposed && !seen[m.Batch] {
			seen[m.Batch] = true
			if _, err := store.New(s.db).DecideRebalanceBatch(ctx, store.DecideRebalanceBatchParams{Batch: m.Batch, Status: MoveApproved}); err != nil {
				return err
			}
		}
	}
	return nil
}

// DecideBatch approves or rejects a proposed rebalance batch.
func (s *Service) DecideBatch(ctx context.Context, batch uuid.UUID, approve bool) (int64, error) {
	status := MoveRejected
	if approve {
		status = MoveApproved
	}
	n, err := store.New(s.db).DecideRebalanceBatch(ctx, store.DecideRebalanceBatchParams{Batch: batch, Status: status})
	if err == nil && n == 0 {
		return 0, ErrNotFound
	}
	return n, err
}

// sharedLoad is a shared node's disk for the rebalancer.
type sharedLoad struct {
	node  store.CapacityNodesRow
	total float64
	used  float64
}

// Rebalance proposes moves that even out shared nodes' disk use in each
// region (V3 §5.3, weekly): from the fullest node to the emptiest, the
// largest projects that narrow the gap, at most ten moves a batch. It
// returns the batch (uuid.Nil if nothing needs moving).
func (s *Service) Rebalance(ctx context.Context) (uuid.UUID, int, error) {
	st, err := s.Settings(ctx)
	if err != nil {
		return uuid.Nil, 0, err
	}
	q := store.New(s.db)
	ns, err := q.CapacityNodes(ctx)
	if err != nil {
		return uuid.Nil, 0, err
	}
	regions := map[string][]*sharedLoad{}
	for _, n := range ns {
		m := hostMetrics(n.Capacity)
		if n.Lifecycle != "active" || n.Status != "healthy" || !n.HasSharedCluster || (n.Role != "shared" && n.Role != "both") || m.DiskTotalBytes <= 0 {
			continue
		}
		regions[n.Region] = append(regions[n.Region], &sharedLoad{node: n, total: float64(m.DiskTotalBytes), used: float64(m.DiskTotalBytes - m.DiskFreeBytes)})
	}
	batch := uuid.New()
	count := 0
	for region, loads := range regions {
		if len(loads) < 2 {
			continue
		}
		for range 10 {
			sort.Slice(loads, func(i, j int) bool { return loads[i].used/loads[i].total > loads[j].used/loads[j].total })
			hi, lo := loads[0], loads[len(loads)-1]
			gap := hi.used/hi.total - lo.used/lo.total
			if gap <= st.RebalanceSpread {
				break
			}
			ps, err := q.NodeProjectSizes(ctx, hi.node.ID)
			if err != nil {
				return batch, count, err
			}
			loC, err := q.SharedInstanceOnNode(ctx, lo.node.ID)
			if err != nil {
				break
			}
			// The largest project that doesn't overshoot: after the move the
			// gap shrinks rather than flips.
			moved := false
			for _, p := range ps {
				if p.Tier != provision.TierShared || p.SizeBytes <= 0 || p.PgVersion != loC.PgVersion || p.ClusterOrg != nil || loC.OrgID != nil {
					continue
				}
				after := (hi.used-p.SizeBytes)/hi.total - (lo.used+p.SizeBytes)/lo.total
				if math.Abs(after) >= gap {
					continue
				}
				to := lo.node.ID
				_, err := q.InsertRebalanceMove(ctx, store.InsertRebalanceMoveParams{
					Batch: batch, Kind: MoveRebalance, ProjectID: p.ID, FromNode: hi.node.ID, ToNode: &to, Status: MoveProposed,
					Reason: fmt.Sprintf("%s: %s is %.0f%% full, %s %.0f%%", region, hi.node.Name, hi.used/hi.total*100, lo.node.Name, lo.used/lo.total*100),
				})
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return batch, count, err
				}
				hi.used -= p.SizeBytes
				lo.used += p.SizeBytes
				count++
				moved = true
				break
			}
			if !moved {
				break
			}
		}
	}
	if count == 0 {
		return uuid.Nil, 0, nil
	}
	s.log.Info("rebalance proposed", "batch", batch, "moves", count)
	return batch, count, nil
}

// sweepEmpty records when nodes became empty and deletes provider servers
// that stayed empty (V3 §5.3). Manual machines are only flagged: someone
// decommissions them. Nodes marked keep are left alone.
//
// Deleting takes two sweeps. The first takes the node out of placement
// (lifecycle retiring, startRetire); after RetireGrace, which lets
// anything placed there just before land, the second counts again and
// retires the node's clusters and deletes its server, or puts the node
// back if something is there (finishRetire). Both count under the node's
// row lock, which a new project's placement holds too.
func (s *Service) sweepEmpty(ctx context.Context) error {
	st, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	q := store.New(s.db)
	occ, err := q.NodeOccupancy(ctx)
	if err != nil {
		return err
	}
	ns, err := q.CapacityNodes(ctx)
	if err != nil {
		return err
	}
	byID := map[uuid.UUID]store.CapacityNodesRow{}
	for _, n := range ns {
		byID[n.ID] = n
	}
	now := s.cfg.Now()
	var errs []error
	for _, o := range occ {
		n, ok := byID[o.ID]
		if !ok || n.Lifecycle == "provisioning" || n.Role == "edge" {
			continue // an edge node holds no databases: it is never "empty"
		}
		if n.Lifecycle == NodeRetiring {
			if n.RetiringAt == nil || now.Sub(*n.RetiringAt) >= s.cfg.RetireGrace {
				errs = append(errs, s.finishRetire(ctx, n.ID, n.Name))
			}
			continue
		}
		empty := isEmpty(store.NodeOccupancyOfRow{Projects: o.Projects, Dedicated: o.Dedicated, Retired: o.Retired, Members: o.Members, Etcd: o.Etcd})
		switch {
		case !empty && n.EmptySince != nil:
			errs = append(errs, q.SetNodeEmptySince(ctx, store.SetNodeEmptySinceParams{ID: n.ID}))
		case empty && n.EmptySince == nil:
			errs = append(errs, q.SetNodeEmptySince(ctx, store.SetNodeEmptySinceParams{ID: n.ID, EmptySince: &now}))
		case empty && !n.Keep && n.Provider != cloud.Manual && n.ProviderServerID != nil &&
			now.Sub(*n.EmptySince) >= time.Duration(st.DeleteEmptyAfterHours)*time.Hour:
			errs = append(errs, s.startRetire(ctx, n.ID, n.Name))
		}
	}
	return errors.Join(errs...)
}

// NodeRetiring is the lifecycle of an empty node taken out of placement
// before its server is deleted.
const NodeRetiring = "retiring"

func isEmpty(o store.NodeOccupancyOfRow) bool {
	return o.Projects == 0 && o.Dedicated == 0 && o.Retired == 0 && o.Members == 0 && o.Etcd == 0
}

// startRetire takes an empty node out of placement, after counting what
// is on it again under its lock: from then on nothing new is placed there.
func (s *Service) startRetire(ctx context.Context, id uuid.UUID, name string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.LockNodeForRetire(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if n.Keep || (n.Lifecycle != "active" && n.Lifecycle != "draining") {
			return nil
		}
		o, err := q.NodeOccupancyOf(ctx, id)
		if err != nil {
			return err
		}
		if !isEmpty(o) {
			return q.SetNodeEmptySince(ctx, store.SetNodeEmptySinceParams{ID: id})
		}
		now := s.cfg.Now()
		if _, err := q.SetNodeRetiring(ctx, store.SetNodeRetiringParams{ID: id, RetiringAt: &now}); err != nil {
			return err
		}
		s.log.Info("retiring empty node: nothing new is placed on it, and its server is deleted", "node", name, "after", s.cfg.RetireGrace)
		return nil
	})
}

// finishRetire deletes a retiring node's server once nothing is on it:
// under the node's lock it counts again and retires the node's shared
// clusters, then deletes the server, then removes the node. If something
// was placed on it meanwhile, or it is now to be kept, it goes back into
// service instead. A server delete that fails is tried again by the next
// sweep (the node stays retiring, its clusters retired).
func (s *Service) finishRetire(ctx context.Context, id uuid.UUID, name string) error {
	serverID := ""
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.LockNodeForRetire(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if n.Lifecycle != NodeRetiring {
			return nil // drained, or put back by hand, meanwhile
		}
		o, err := q.NodeOccupancyOf(ctx, id)
		if err != nil {
			return err
		}
		if !isEmpty(o) || n.Keep || n.Provider == cloud.Manual || n.ProviderServerID == nil {
			if _, err := q.ReturnRetiringNode(ctx, id); err != nil {
				return err
			}
			s.log.Warn("empty node not deleted: something was placed on it, or it is kept; it is back in service",
				"node", name, "projects", o.Projects, "dedicated", o.Dedicated, "retired", o.Retired, "members", o.Members, "etcd", o.Etcd, "keep", n.Keep)
			return nil
		}
		if err := q.RetireNodeSharedClusters(ctx, id); err != nil {
			return err
		}
		serverID = *n.ProviderServerID
		return nil
	})
	if err != nil || serverID == "" {
		return err
	}
	if err := s.provider.DeleteServer(ctx, serverID); err != nil {
		return fmt.Errorf("delete server %s of %s: %w", serverID, name, err)
	}
	if err := s.nodes.RemoveNode(ctx, id); err != nil {
		return err
	}
	s.log.Info("deleted empty node", "node", name, "server", serverID)
	return nil
}
