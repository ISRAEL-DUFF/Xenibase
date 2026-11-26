package dedicated

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Read replicas (V4 §7): streaming members of the instance's Patroni
// cluster, tagged nofailover and nosync, on other nodes (possibly in other
// regions). They follow whichever member leads, so a failover moves them
// along with it; the pooler's <db>_ro route balances reads across the ones
// keeping up.
const (
	KindCreateReplica = "create_replica"
	KindDeleteReplica = "delete_replica"
	KindDetachReplica = "detach_replica"

	// MaxReplicas is how many read replicas a project may have (V4 §7).
	MaxReplicas = 2
	// DefaultReplicaMaxLag is how far behind a replica may fall and stay
	// in rotation.
	DefaultReplicaMaxLag = 10 * time.Second
)

// Replica statuses.
const (
	ReplicaCreating  = "creating"
	ReplicaStreaming = "streaming"
	ReplicaLagging   = "lagging"
	ReplicaDown      = "down"
	ReplicaDeleting  = "deleting"
	ReplicaDetaching = "detaching"
	ReplicaFailed    = "failed"
)

// ReplicaParams asks for a read replica.
type ReplicaParams struct {
	ProjectID uuid.UUID
	// NodeID places it (default: the least loaded eligible node in Region).
	NodeID *uuid.UUID
	// Region is where it goes (default: the project's). A project with data
	// residency keeps its replicas in its region.
	Region    string
	CreatedBy *uuid.UUID
	// Settings are its standby settings (nil: Postgres's defaults).
	Settings *ReplicaSettings
}

type replicaParams struct {
	Instance uuid.UUID `json:"instance"`
	Replica  uuid.UUID `json:"replica"`
	Node     uuid.UUID `json:"node"`
	Region   string    `json:"region,omitempty"`
	// Settings are absent from operations queued before they existed.
	Settings *ReplicaSettings `json:"settings,omitempty"`
}

// replicaPort is the host port an instance's replicas ask for when they
// are published on their nodes' addresses: one per instance, so that the
// pooler reaches all of them on the same port.
func replicaPort(instance uuid.UUID) int {
	h := fnv.New32a()
	_, _ = h.Write(instance[:])
	return 20000 + int(h.Sum32()%10000)
}

// replicaNode picks where a replica goes: a healthy node that takes
// dedicated instances, in region, holding no member of the instance.
func (s *Service) replicaNode(ctx context.Context, q *store.Queries, inst store.Instance, region string, want *uuid.UUID) (store.Node, error) {
	ns, err := q.ListNodes(ctx)
	if err != nil {
		return store.Node{}, err
	}
	members, err := q.ListInstanceMembers(ctx, inst.ID)
	if err != nil {
		return store.Node{}, err
	}
	taken := map[uuid.UUID]bool{inst.NodeID: true}
	for _, m := range members {
		taken[m.NodeID] = true
	}
	var best *store.Node
	bestCount := 1 << 30
	for i, n := range ns {
		eligible := !taken[n.ID] && n.AgentCertFp != nil && n.Status == "healthy" && (n.Role == "dedicated" || n.Role == "both") && n.Region == region
		if want != nil {
			if n.ID != *want {
				continue
			}
			if !eligible {
				return store.Node{}, fmt.Errorf("%w: a replica needs a healthy node in %s that takes dedicated instances and holds no other member of this project's instance; %s doesn't qualify", provision.ErrConflict, region, n.Name)
			}
			return n, nil
		}
		if !eligible {
			continue
		}
		insts, err := q.ListNodeInstances(ctx, n.ID)
		if err != nil {
			return store.Node{}, err
		}
		if len(insts) < bestCount {
			best, bestCount = &ns[i], len(insts)
		}
	}
	if want != nil {
		return store.Node{}, fmt.Errorf("%w: no such node", provision.ErrConflict)
	}
	if best == nil {
		return store.Node{}, fmt.Errorf("%w: a replica needs another healthy node in %s that takes dedicated instances", provision.ErrConflict, region)
	}
	return *best, nil
}

// CreateReplica checks and queues a read replica.
func (s *Service) CreateReplica(ctx context.Context, p ReplicaParams) (store.Operation, error) {
	rs := DefaultReplicaSettings
	if p.Settings != nil {
		rs = *p.Settings
	}
	if err := rs.Validate(); err != nil {
		return store.Operation{}, err
	}
	if s.Etcd == nil {
		return store.Operation{}, fmt.Errorf("%w: read replicas need the etcd service (as HA does)", provision.ErrNoDedicated)
	}
	return s.projects.EnqueueExclusiveTx(ctx, p.ProjectID, []string{provision.StatusActive}, "", KindCreateReplica, p.CreatedBy,
		func(tx pgx.Tx, pr store.Project) (any, error) {
			if pr.Tier != provision.TierDedicated {
				return nil, fmt.Errorf("%w: read replicas are for dedicated projects; promote this one first", provision.ErrConflict)
			}
			q := store.New(tx)
			inst, err := q.GetInstance(ctx, pr.InstanceID)
			if err != nil {
				return nil, err
			}
			if inst.Status != "running" {
				return nil, fmt.Errorf("%w: the instance is %s", provision.ErrConflict, inst.Status)
			}
			// A replica is an instance the size of its primary (V4 §7),
			// within the dedicated allowance, counted under the
			// organisation's lock with the replicas queued before it.
			size := SizeProfile(inst)
			claim := provision.Claim{OrgID: pr.OrgID, Instance: size}
			if inst.VolumeGb != nil {
				claim.VolumeGB = int(*inst.VolumeGb)
			}
			if err := s.projects.Claim(ctx, tx, claim); err != nil {
				return nil, err
			}
			n, err := q.CountProjectReplicas(ctx, pr.ID)
			if err != nil {
				return nil, err
			}
			if n >= MaxReplicas {
				return nil, fmt.Errorf("%w: a project has at most %d read replicas", provision.ErrConflict, MaxReplicas)
			}
			region := p.Region
			if region == "" {
				region = pr.Region
			}
			if pr.DataResidency && region != pr.Region {
				return nil, fmt.Errorf("%w: the project keeps its data in %s (data residency); its replicas stay there too", provision.ErrConflict, pr.Region)
			}
			etcdRegion, err := s.etcdRegion(ctx, inst)
			if err != nil {
				return nil, err
			}
			if err := s.Etcd.Ready(ctx, etcdRegion); err != nil {
				return nil, err
			}
			node, err := s.replicaNode(ctx, q, inst, region, p.NodeID)
			if err != nil {
				return nil, err
			}
			return replicaParams{Instance: inst.ID, Replica: uuid.New(), Node: node.ID, Region: region, Settings: &rs}, nil
		})
}

func (s *Service) runCreateReplica(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params replicaParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return err
	}
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, params.Instance)
	if err != nil {
		return jobs.Permanent(err)
	}
	p, err := q.GetProject(ctx, *op.ProjectID)
	if err != nil {
		return jobs.Permanent(err)
	}
	// 1. The instance under Patroni, as for HA (a restart with the poolers
	// holding clients); one that already is needs nothing.
	if !inst.Patroni {
		if inst.EtcdRegion == nil {
			region, err := s.etcdRegion(ctx, inst)
			if err != nil {
				return err
			}
			if err := q.SetInstanceEtcdRegion(ctx, store.SetInstanceEtcdRegionParams{ID: inst.ID, EtcdRegion: &region}); err != nil {
				return err
			}
			inst.EtcdRegion = &region
		}
		if err := s.preparePatroni(ctx, &inst, false, log); err != nil {
			return err
		}
		if err := s.convertToPatroni(ctx, p, inst, log); err != nil {
			return err
		}
		if inst, err = q.GetInstance(ctx, inst.ID); err != nil {
			return err
		}
	}

	// 2. The replica: a member tagged nofailover/nosync, built like a
	// standby from the newest base backup.
	if _, err := q.GetReadReplica(ctx, params.Replica); errors.Is(err, pgx.ErrNoRows) {
		if _, err := q.InsertInstanceMember(ctx, store.InsertInstanceMemberParams{ID: params.Replica, InstanceID: inst.ID, NodeID: params.Node, Role: "starting"}); err != nil {
			return err
		}
		if err := q.SetMemberReplica(ctx, params.Replica); err != nil {
			return err
		}
		size := DefaultProfile
		if inst.Profile != nil {
			size = *inst.Profile
		}
		var region *string
		if params.Region != "" {
			region = &params.Region
		}
		rs := DefaultReplicaSettings
		if params.Settings != nil {
			rs = *params.Settings
		}
		if _, err := q.InsertReadReplica(ctx, store.InsertReadReplicaParams{ID: params.Replica, ProjectID: p.ID, InstanceMemberID: &params.Replica,
			NodeID: params.Node, RegionID: region, Size: size, HotStandbyFeedback: rs.HotStandbyFeedback, MaxStandbyDelayS: int32(rs.MaxStandbyDelay)}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	agent, err := s.nodes.ForNode(ctx, params.Node)
	if err != nil {
		return err
	}
	spec, err := s.memberSpec(ctx, inst, params.Replica)
	if err != nil {
		return err
	}
	if err := log.Info(ctx, "replica", "starting a read replica on node %s from the newest base backup", agent.Node.Name); err != nil {
		return err
	}
	start := time.Now()
	res, err := agent.CreateInstance(ctx, spec)
	if err != nil {
		return fmt.Errorf("replica on %s: %w", agent.Node.Name, err)
	}
	if err := s.recordMember(ctx, params.Replica, agent, res); err != nil {
		return err
	}
	m, err := s.waitMember(ctx, inst.ID, params.Replica, false, s.standbyTimeout(), log)
	if err != nil {
		return err
	}
	if err := s.refreshMembers(ctx, inst); err != nil {
		return err
	}
	if err := q.SetReplicaStatus(ctx, store.SetReplicaStatusParams{ID: params.Replica, Status: ReplicaStreaming}); err != nil {
		return err
	}
	// Its spec carried the standby settings; confirm them (the watcher
	// retries if this fails).
	if r, err := q.GetReadReplica(ctx, params.Replica); err == nil {
		s.applyReplicaSettings(ctx, inst, r)
		if r, err = q.GetReadReplica(ctx, params.Replica); err == nil && r.SettingsError == nil {
			fb := "off"
			if r.HotStandbyFeedback {
				fb = "on"
			}
			if err := log.Info(ctx, "settings", "hot_standby_feedback %s, queries conflicting with replay wait up to %d s", fb, r.MaxStandbyDelayS); err != nil {
				return err
			}
		}
	}
	// 3. Into rotation once it is caught up: the watcher checks its lag
	// every poll; this is the first check.
	if err := s.checkReplicas(ctx, inst, p); err != nil {
		_ = log.Warn(ctx, "rotation", "the first lag check failed (the watcher retries): %v", err)
	}
	route := "postgresql://…/" + p.DbName + "_ro"
	return log.Info(ctx, "done", "read replica on %s is %s (%s behind) after %s; reads through %s go to it once it keeps up",
		agent.Node.Name, m.State, lagText(m.LagBytes()), time.Since(start).Round(time.Second), route)
}

// failCreateReplica removes a replica that never joined.
func (s *Service) failCreateReplica(ctx context.Context, op store.Operation, log *jobs.StepLogger, cause error) error {
	var params replicaParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return err
	}
	if err := s.removeMember(ctx, params.Replica, params.Node); err != nil {
		return err
	}
	q := store.New(s.db)
	if _, err := q.GetReadReplica(ctx, params.Replica); err == nil {
		msg := cause.Error()
		_ = q.SetReplicaStatus(ctx, store.SetReplicaStatusParams{ID: params.Replica, Status: ReplicaFailed, Error: &msg})
		if err := q.DeleteReadReplica(ctx, params.Replica); err != nil {
			return err
		}
	}
	return log.Warn(ctx, "rollback", "the read replica was not created: %v. The project keeps running as before.", cause)
}

// DeleteReplica queues removing a read replica.
func (s *Service) DeleteReplica(ctx context.Context, projectID, replica uuid.UUID, by *uuid.UUID) (store.Operation, error) {
	return s.projects.EnqueueExclusiveTx(ctx, projectID, []string{provision.StatusActive, provision.StatusError}, "", KindDeleteReplica, by,
		func(tx pgx.Tx, pr store.Project) (any, error) {
			r, err := store.New(tx).GetReadReplica(ctx, replica)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (r.ProjectID != pr.ID || r.DeletedAt != nil)) {
				return nil, provision.ErrNotFound
			}
			if err != nil {
				return nil, err
			}
			if r.Status == ReplicaDetaching {
				return nil, fmt.Errorf("%w: the replica is being detached", provision.ErrConflict)
			}
			return replicaParams{Instance: pr.InstanceID, Replica: r.ID, Node: r.NodeID}, nil
		})
}

func (s *Service) runDeleteReplica(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params replicaParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return err
	}
	q := store.New(s.db)
	r, err := q.GetReadReplica(ctx, params.Replica)
	if err != nil {
		return jobs.Permanent(err)
	}
	// Out of the read route first, so no new reads land on it.
	if r.DeletedAt == nil {
		if err := q.SetReplicaLag(ctx, store.SetReplicaLagParams{ID: r.ID, Status: ReplicaDeleting, InRotation: false, LagBytes: r.LagBytes, LagMs: r.LagMs}); err != nil {
			return err
		}
		if err := s.projects.SyncPooler(ctx, log, "pooler", "read replica leaving the read route"); err != nil {
			return err
		}
	}
	if err := s.removeMember(ctx, r.ID, r.NodeID); err != nil {
		return err
	}
	if err := q.DeleteReadReplica(ctx, r.ID); err != nil {
		return err
	}
	return log.Info(ctx, "done", "read replica removed")
}

// IsReplica reports whether a member is a read replica.
func isReplica(m Member) bool { return m.Replica }

// haMembers drops the read replicas: what HA's own logic (standbys,
// switchovers, failure domains) works with.
func haMembers(ms []Member) []Member {
	out := make([]Member, 0, len(ms))
	for _, m := range ms {
		if !isReplica(m) {
			out = append(out, m)
		}
	}
	return out
}

// HasReplicas reports whether a project has read replicas; operations that
// rebuild its instance elsewhere refuse until they are deleted.
func (s *Service) HasReplicas(ctx context.Context, q *store.Queries, project uuid.UUID) error {
	n, err := q.CountProjectReplicas(ctx, project)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: delete the project's read replicas first", provision.ErrConflict)
	}
	return nil
}

// ReplicaMaxLag is how far behind a replica may fall and stay in rotation.
func (s *Service) ReplicaMaxLag() time.Duration { return s.cfg.ReplicaMaxLag }
