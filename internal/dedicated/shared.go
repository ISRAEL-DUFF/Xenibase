package dedicated

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// KindSharedCluster creates a shared cluster on a node through its agent,
// for multi-node shared placement (M4).
const KindSharedCluster = "shared_cluster"

// Kinds returns the operation kinds this service runs.
func (s *Service) Kinds() map[string]jobs.Kind {
	return map[string]jobs.Kind{
		KindSharedCluster:     {Handler: s.runSharedCluster, OnFail: s.failSharedCluster, MaxAttempts: 3},
		KindPromote:           {Handler: s.runPromote, OnFail: s.failPromote, MaxAttempts: 2, Timeout: 12 * time.Hour},
		KindDemote:            {Handler: s.runDemote, OnFail: s.failDemote, MaxAttempts: 2, Timeout: 12 * time.Hour},
		KindMove:              {Handler: s.runMove, OnFail: s.failMove, MaxAttempts: 2, Timeout: 48 * time.Hour},
		KindUpgrade:           {Handler: s.runMove, OnFail: s.failMove, MaxAttempts: 2, Timeout: 48 * time.Hour},
		KindHAEnable:          {Handler: s.runHAEnable, OnFail: s.failHAEnable, MaxAttempts: 2, Timeout: 24 * time.Hour},
		KindHADisable:         {Handler: s.runHADisable, MaxAttempts: 3},
		KindHASwitchover:      {Handler: s.runSwitchover, MaxAttempts: 1, Timeout: 5 * time.Minute},
		KindHAEtcdMove:        {Handler: s.runHAEtcdMove, MaxAttempts: 2, Timeout: 24 * time.Hour},
		KindRegionEtcdMoveAll: {Handler: s.runRegionEtcdMoveAll, MaxAttempts: 1, Timeout: 7 * 24 * time.Hour},
		KindCreateReplica:     {Handler: s.runCreateReplica, OnFail: s.failCreateReplica, MaxAttempts: 2, Timeout: 24 * time.Hour},
		KindDeleteReplica:     {Handler: s.runDeleteReplica, MaxAttempts: 3},
		KindDetachReplica:     {Handler: s.runDetachReplica, OnFail: s.failDetachReplica, MaxAttempts: 2, Timeout: 6 * time.Hour},
		KindResize:            {Handler: s.runResize, OnFail: s.failResize, MaxAttempts: 2, Timeout: 2 * time.Hour},
		KindMinorUpgrade:      {Handler: s.runMinorUpgrade, OnFail: s.failMinorUpgrade, MaxAttempts: 1, Timeout: 3 * time.Hour},
	}
}

// sharedSettings is postgresql.conf for an agent-run shared cluster (spec
// §11.2, scaled to the memory given).
func sharedSettings(memMB int) map[string]string {
	mb := func(n int) string { return strconv.Itoa(n) + "MB" }
	return map[string]string{
		"max_connections":            "500",
		"shared_buffers":             mb(memMB / 4),
		"effective_cache_size":       mb(memMB * 3 / 4),
		"work_mem":                   "8MB",
		"maintenance_work_mem":       mb(min(max(memMB/32, 64), 1024)),
		"wal_compression":            "on",
		"checkpoint_timeout":         "15min",
		"autovacuum_max_workers":     "5",
		"shared_preload_libraries":   "pg_stat_statements",
		"log_min_duration_statement": "5s",
		// Logical-replication moves (V3 §2.3), and a bound on the WAL a
		// slot may keep: a move's slot left behind would otherwise fill the
		// node's disk and stop every tenant on the cluster.
		"wal_level":              "logical",
		"max_slot_wal_keep_size": sharedSlotWALCap,
	}
}

// sharedSlotWALCap is the most WAL a replication slot may hold back on an
// agent-run shared cluster.
const sharedSlotWALCap = "10GB"

// sharedSpec is the agent's spec for an agent-run shared cluster.
func sharedSpec(inst store.Instance, secret provision.AdminSecret) agentapi.InstanceSpec {
	mem := 1024
	if inst.MemLimitMb != nil {
		mem = int(*inst.MemLimitMb)
	}
	return agentapi.InstanceSpec{
		ID: inst.ID.String(), Kind: agentapi.InstanceShared, MemoryMB: mem,
		AdminUser: secret.User, AdminPassword: secret.Password, Settings: sharedSettings(mem),
		PGVersion: int(inst.PgVersion),
	}
}

// checkPGVersion resolves a requested major against the supported ones
// (without a projects service, as in unit tests, it takes v as given).
func (s *Service) checkPGVersion(ctx context.Context, v int) (int, error) {
	if s.projects == nil {
		return v, nil
	}
	return s.projects.CheckVersion(ctx, v, provision.ForCluster)
}

// AddSharedCluster records a shared cluster on node and queues its
// creation. memoryMB sizes it (its CPU is not limited: it is the node's
// shared tenant pool).
func (s *Service) AddSharedCluster(ctx context.Context, nodeID uuid.UUID, memoryMB, pgVersion int, by *uuid.UUID) (store.Operation, error) {
	if memoryMB < 512 || memoryMB > 1<<20 {
		return store.Operation{}, fmt.Errorf("%w: memory must be 512 MB to 1 TB", provision.ErrInvalid)
	}
	var op store.Operation
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.GetNode(ctx, nodeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return provision.ErrNotFound
		}
		if err != nil {
			return err
		}
		if n.Role != "shared" && n.Role != "both" {
			return fmt.Errorf("%w: node %s does not take shared projects (role %s)", provision.ErrInvalid, n.Name, n.Role)
		}
		if n.AgentCertFp == nil {
			return fmt.Errorf("%w: node %s has no agent yet", provision.ErrConflict, n.Name)
		}
		if n.Status != "healthy" {
			return fmt.Errorf("%w: node %s is %s; its agent has not answered recently", provision.ErrConflict, n.Name, n.Status)
		}
		if _, err := q.SharedInstanceOnNode(ctx, nodeID); err == nil {
			return fmt.Errorf("%w: node %s already has a shared cluster", provision.ErrConflict, n.Name)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if pgVersion, err = s.checkPGVersion(ctx, pgVersion); err != nil {
			return err
		}
		mem := int32(memoryMB)
		name := "shared"
		inst, err := q.InsertInstance(ctx, store.InsertInstanceParams{
			ID: uuid.New(), NodeID: nodeID, Kind: provision.TierShared, PgVersion: int32(pgVersion), MemLimitMb: &mem, Profile: &name,
		})
		if err != nil {
			return err
		}
		op, err = jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: KindSharedCluster, CreatedBy: by,
			Params: map[string]any{"instance_id": inst.ID, "node_id": nodeID}})
		return err
	})
	return op, err
}

type sharedParams struct {
	InstanceID uuid.UUID `json:"instance_id"`
}

func (s *Service) runSharedCluster(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params sharedParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return err
	}
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, params.InstanceID)
	if err != nil {
		return jobs.Permanent(err)
	}
	if len(inst.AdminSecret) == 0 {
		sealed, err := provision.SealInstanceSecret(s.keyring, inst.ID, provision.AdminSecret{User: "xenibase_admin", Password: randomPassword()})
		if err != nil {
			return err
		}
		if err := q.SetInstanceAdminSecret(ctx, store.SetInstanceAdminSecretParams{ID: inst.ID, AdminSecret: sealed}); err != nil {
			return err
		}
		inst.AdminSecret = sealed
	}
	secret, err := provision.OpenInstanceSecret(s.keyring, inst.ID, inst.AdminSecret)
	if err != nil {
		return jobs.Permanent(err)
	}
	spec := sharedSpec(inst, secret)
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return err
	}
	if err := log.Info(ctx, "instance", "starting a shared cluster (%d MB) on node %s", spec.MemoryMB, agent.Node.Name); err != nil {
		return err
	}
	res, err := agent.CreateInstance(ctx, spec)
	if err != nil {
		return err
	}
	if res.Host == "" {
		return jobs.Permanent(fmt.Errorf("agent on %s reports no address for the instance", agent.Node.Name))
	}
	run := store.SetInstanceRunningParams{ID: inst.ID, ContainerID: &res.ContainerID, Host: &res.Host, Port: int32(res.Port)}
	if s.cfg.AdminVia == "published" && res.PublishedPort > 0 {
		host := res.PublishedHost
		if host == "" || host == "0.0.0.0" {
			host = agent.Node.PrivateAddr
		}
		port := int32(res.PublishedPort)
		run.AdminHost, run.AdminPort = &host, &port
	}
	if err := q.SetInstanceRunning(ctx, run); err != nil {
		return err
	}
	inst, err = q.GetInstance(ctx, inst.ID)
	if err != nil {
		return err
	}
	if err := s.adoptRestore(ctx, inst, store.Project{}, nil, log); err != nil { // hardening only
		return err
	}
	return log.Info(ctx, "done", "shared cluster on %s is running; new shared projects are placed on the least loaded cluster", agent.Node.Name)
}

func (s *Service) failSharedCluster(ctx context.Context, op store.Operation, log *jobs.StepLogger, _ error) error {
	var params sharedParams
	if err := decodeJSON(op.Params, &params); err != nil {
		return err
	}
	inst, err := store.New(s.db).GetInstance(ctx, params.InstanceID)
	if err != nil {
		return err
	}
	if err := s.rollbackSharedCluster(ctx, inst); err != nil {
		_ = log.Warn(ctx, "rollback", "could not remove the half-created shared cluster: %v; it will be removed automatically once the node is reachable", err)
		return err
	}
	return log.Warn(ctx, "rollback", "removed the half-created shared cluster")
}

// rollbackSharedCluster removes a shared cluster whose creation failed. The
// instance is recorded as deleted only once its node has destroyed it;
// otherwise a container the node did create would be left running with
// nothing in the metadata DB pointing at it. A failed removal marks the
// instance "error", which ReapOrphans retries until the node answers.
func (s *Service) rollbackSharedCluster(ctx context.Context, inst store.Instance) error {
	q := store.New(s.db)
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err == nil {
		err = agent.DestroyInstance(ctx, inst.ID.String())
	}
	if err != nil {
		msg := err.Error()
		if serr := q.SetInstanceStatus(context.WithoutCancel(ctx), store.SetInstanceStatusParams{ID: inst.ID, Status: "error", Error: &msg}); serr != nil {
			return errors.Join(err, serr)
		}
		return err
	}
	return q.MarkInstanceDeleted(ctx, inst.ID)
}

// Instance actions.
const (
	ActionStart   = "start"
	ActionStop    = "stop"
	ActionRestart = "restart"
)

// Act starts, stops, or restarts a dedicated project's instance.
func (s *Service) Act(ctx context.Context, p store.Project, action string) (agentapi.Instance, error) {
	if p.Tier != provision.TierDedicated {
		return agentapi.Instance{}, fmt.Errorf("%w: only dedicated projects have their own instance", provision.ErrInvalid)
	}
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, p.InstanceID)
	if err != nil {
		return agentapi.Instance{}, err
	}
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return agentapi.Instance{}, err
	}
	if inst.HaEnabled {
		// Stopping or restarting one member would fail over (V3 §2.2).
		return agentapi.Instance{}, fmt.Errorf("%w: this project has HA; use a switchover, or turn HA off first", provision.ErrConflict)
	}
	id := agentKey(inst)
	var res agentapi.Instance
	switch action {
	case ActionStop:
		res, err = agent.StopInstance(ctx, id)
		if err == nil {
			err = q.SetInstanceStatus(ctx, store.SetInstanceStatusParams{ID: inst.ID, Status: "stopped"})
		}
	case ActionRestart:
		// A fresh container, so a rebuilt image (a Postgres minor
		// release) takes effect.
		res, err = s.Recreate(ctx, inst)
		if err == nil {
			err = q.SetInstanceStatus(ctx, store.SetInstanceStatusParams{ID: inst.ID, Status: "running"})
		}
	case ActionStart:
		res, err = agent.StartInstance(ctx, id)
		if err == nil {
			// A restarted container may be published on a new port.
			err = s.recordRunning(ctx, inst, agent, res)
		}
		if err == nil {
			err = q.SetInstanceStatus(ctx, store.SetInstanceStatusParams{ID: inst.ID, Status: "running"})
		}
	default:
		return res, fmt.Errorf("%w: action must be start, stop, or restart", provision.ErrInvalid)
	}
	return res, err
}

// Status asks the agent for the instance's container state.
func (s *Service) Status(ctx context.Context, inst store.Instance) (agentapi.Instance, error) {
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return agentapi.Instance{}, err
	}
	return agent.Instance(ctx, agentKey(inst))
}

func decodeJSON(raw []byte, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return jobs.Permanent(fmt.Errorf("operation params: %w", err))
	}
	return nil
}
