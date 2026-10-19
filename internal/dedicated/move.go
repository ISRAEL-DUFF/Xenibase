package dedicated

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/logical"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/pgverify"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// KindMove moves a project to another node, on the same tier and Postgres
// version, by logical replication with a dump/restore fallback (V3 §2.3).
const KindMove = "logical_move"

// Retired copies a node move keeps for 48 hours: a shared database, or a
// whole dedicated instance.
const (
	retiredMoveCopy     = "move"
	retiredMoveInstance = "move_instance"
)

// Move modes.
const (
	ModeLogical = "logical"
	ModeDump    = "dump"
)

// cutoverTimeout bounds the wait for the last commits during the freeze.
const cutoverTimeout = 30 * time.Second

// MoveParams asks for a node move.
type MoveParams struct {
	ProjectID uuid.UUID
	NodeID    uuid.UUID
	CreatedBy *uuid.UUID
	// Size, for a dedicated project, is the new instance's size when the
	// move is a resize that didn't fit its node (V4.1 §5.1); nil keeps it.
	Size *Size
}

type moveParams struct {
	TargetInstance uuid.UUID `json:"target_instance"`
	SourceInstance uuid.UUID `json:"source_instance"`
	// NewInstance: the target was created for this move (a dedicated
	// project), so a rollback destroys it.
	NewInstance bool `json:"new_instance"`
	// Resized: the target is a new size; the disk warning follows its
	// volume when the move finishes.
	Resized bool `json:"resized,omitempty"`
}

func opMove(op store.Operation) (moveParams, error) {
	var p moveParams
	if err := json.Unmarshal(op.Params, &p); err != nil || p.TargetInstance == uuid.Nil {
		return p, jobs.Permanent(fmt.Errorf("move params: %w", err))
	}
	return p, nil
}

// Move checks a node move and queues it. A shared project goes to the
// shared cluster on the node, which must run the same Postgres version and
// be open to the project's organisation; a dedicated project gets a new
// instance of the same size there.
func (s *Service) Move(ctx context.Context, mp MoveParams) (store.Operation, error) {
	return s.projects.EnqueueExclusiveTx(ctx, mp.ProjectID, []string{provision.StatusActive}, provision.StatusMoving, KindMove, mp.CreatedBy,
		func(tx pgx.Tx, pr store.Project) (any, error) {
			q := store.New(tx)
			src, err := q.GetInstance(ctx, pr.InstanceID)
			if err != nil {
				return nil, err
			}
			if src.HaEnabled {
				return nil, fmt.Errorf("%w: turn HA off before moving this project (a switchover moves the primary between its nodes)", provision.ErrConflict)
			}
			if err := s.HasReplicas(ctx, q, pr.ID); err != nil {
				return nil, err
			}
			if src.NodeID == mp.NodeID {
				return nil, fmt.Errorf("%w: the project is already on that node", provision.ErrInvalid)
			}
			if err := checkMoveRegion(ctx, q, pr, mp.NodeID); err != nil {
				return nil, err
			}
			if pr.Tier == provision.TierShared {
				target, err := q.SharedInstanceOnNode(ctx, mp.NodeID)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, fmt.Errorf("%w: that node has no shared cluster", provision.ErrInvalid)
				}
				if err != nil {
					return nil, err
				}
				if target.PgVersion != src.PgVersion {
					return nil, fmt.Errorf("%w: the shared cluster on that node runs Postgres %d, the project %d; upgrade the project instead",
						provision.ErrInvalid, target.PgVersion, src.PgVersion)
				}
				if target.OrgID != nil && *target.OrgID != pr.OrgID {
					return nil, fmt.Errorf("%w: the shared cluster on that node belongs to another organisation", provision.ErrInvalid)
				}
				if target.Status != "running" {
					return nil, fmt.Errorf("%w: the shared cluster on that node is %s", provision.ErrConflict, target.Status)
				}
				return moveParams{TargetInstance: target.ID, SourceInstance: src.ID}, nil
			}
			// The same size there (a custom one included), or the new one.
			size := SizeOf(src)
			if mp.Size != nil {
				size = *mp.Size
			}
			cp := provision.CreateParams{NodeID: &mp.NodeID, Profile: DefaultProfile, VolumeGB: size.DiskGB}
			if _, err := s.Validate(ctx, &cp); err != nil {
				return nil, err
			}
			target := uuid.New()
			prefix := "instances/" + target.String() + "/wal-g"
			mem, vol, name := int32(size.MemoryMB), int32(size.DiskGB), profileName(size)
			if _, err := q.InsertInstance(ctx, store.InsertInstanceParams{
				ID: target, NodeID: mp.NodeID, Kind: provision.TierDedicated, PgVersion: src.PgVersion, CpuLimit: numeric(size.CPUs),
				MemLimitMb: &mem, VolumeGb: &vol, Profile: &name, WalgPrefix: &prefix,
			}); err != nil {
				return nil, err
			}
			if err := q.SetInstancePITRDays(ctx, store.SetInstancePITRDaysParams{ID: target, PitrDays: src.PitrDays}); err != nil {
				return nil, err
			}
			return moveParams{TargetInstance: target, SourceInstance: src.ID, NewInstance: true, Resized: mp.Size != nil}, nil
		})
}

// checkMoveRegion: a move may go to another region's node (a region
// move), except out of a data-residency project's region (V3 §6.3).
func checkMoveRegion(ctx context.Context, q *store.Queries, pr store.Project, nodeID uuid.UUID) error {
	n, err := q.GetNode(ctx, nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no node %s", provision.ErrInvalid, nodeID)
	}
	if err != nil {
		return err
	}
	if n.Region == pr.Region {
		return nil
	}
	if pr.DataResidency {
		return fmt.Errorf("%w: the project's data must stay in %s; turn data residency off before moving it to %s",
			provision.ErrConflict, pr.Region, n.Region)
	}
	r, err := q.GetRegion(ctx, n.Region)
	if err != nil {
		return err
	}
	if r.Status != "active" {
		return fmt.Errorf("%w: region %s is not open for projects", provision.ErrInvalid, r.ID)
	}
	return nil
}

// runMove moves the project's database to the target instance.
func (s *Service) runMove(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	params, err := opMove(op)
	if err != nil {
		return err
	}
	q := store.New(s.db)
	p, err := q.GetProject(ctx, *op.ProjectID)
	if err != nil {
		return jobs.Permanent(err)
	}
	if p.InstanceID == params.TargetInstance {
		// A retry after the cutover committed: only finishing steps remain.
		return s.finishMove(ctx, op, p, params.SourceInstance, log, time.Time{})
	}
	if want := movingStatus(op); p.Status != want {
		return jobs.Permanent(fmt.Errorf("project is %s, not %s", p.Status, want))
	}
	if p.InstanceID != params.SourceInstance {
		return jobs.Permanent(errors.New("the project is no longer on the instance being moved from"))
	}
	pt := p
	pt.InstanceID = params.TargetInstance
	if params.NewInstance {
		if err := s.Ensure(ctx, store.Operation{}, pt, log); err != nil {
			return err
		}
	}
	if err := s.prepareTarget(ctx, op, p, pt, log); err != nil {
		return err
	}
	agent, err := s.nodes.ForInstance(ctx, params.TargetInstance)
	if err != nil {
		return err
	}
	run, err := s.copyForMove(ctx, op, p, pt, agent, log, true)
	if err != nil {
		return err
	}

	// The commit is the point of no return. The source is kept for 48
	// hours: the database on a shared cluster, read-only; a dedicated
	// instance, stopped (its base backups stay restorable until their
	// retention, V2 §5.4).
	reason := retiredMoveCopy
	if p.Tier == provision.TierDedicated {
		reason = retiredMoveInstance
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tq := store.New(tx)
		if err := tq.MoveCutOver(ctx, store.MoveCutOverParams{ID: p.ID, InstanceID: pt.InstanceID}); err != nil {
			return err
		}
		if _, err := tq.InsertRetiredDatabase(ctx, store.InsertRetiredDatabaseParams{
			ProjectID: p.ID, InstanceID: p.InstanceID, DbName: p.DbName, OwnerRole: p.OwnerRole,
			Reason: reason, DropAfter: time.Now().Add(RetainSource),
		}); err != nil {
			return err
		}
		if p.Tier == provision.TierDedicated {
			expires := time.Now().Add(time.Duration(s.cfg.RetainFull) * 24 * time.Hour)
			_, err := tq.ExpireBaseBackups(ctx, store.ExpireBaseBackupsParams{ProjectID: p.ID, ExpiresAt: &expires})
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := log.Info(ctx, "cutover", "route now points at node %s", agent.Node.Name); err != nil {
		return err
	}
	p, err = q.GetProject(ctx, p.ID)
	if err != nil {
		return err
	}
	return s.finishMove(ctx, op, p, params.SourceInstance, log, run.frozeAt)
}

// prepareTarget creates the database and every role on the target with
// the same SCRAM verifiers, starting from an empty database on a retry.
func (s *Service) prepareTarget(ctx context.Context, op store.Operation, p, pt store.Project, log *jobs.StepLogger) error {
	if err := s.projects.Prepare(ctx, pt, log); err != nil {
		return err
	}
	if op.Attempts > 1 {
		// An earlier attempt's subscription must go before its database can.
		s.cleanupMove(ctx, op, p.InstanceID, pt.InstanceID, p.DbName, nil)
		if err := s.projects.RecreateDatabase(ctx, pt, log); err != nil {
			return err
		}
	}
	if err := s.projects.SyncMemberRoles(ctx, pt, log); err != nil {
		return err
	}
	if err := s.restoreIntegratorRoles(ctx, p, pt, log); err != nil {
		return err
	}
	src, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer src.Close(context.Background())
	return s.placeholderRoles(ctx, src, pt, p.OwnerRole, log)
}

// moveRun is a copy that finished with the source's writes frozen.
type moveRun struct {
	mode    string
	frozeAt time.Time
}

// copyForMove copies p's database to pt's instance and returns with writes
// to the source frozen and the target verified, ready for the caller to
// switch the route. It moves by logical replication when the preflight
// allows, and otherwise by dump/restore during the freeze (allowDump) or
// not at all.
func (s *Service) copyForMove(ctx context.Context, op store.Operation, p, pt store.Project, agent *nodes.Agent, log *jobs.StepLogger, allowDump bool) (moveRun, error) {
	var run moveRun
	conn := func(inst uuid.UUID, db string) (*pgx.Conn, error) { return s.projects.AdminConn(ctx, inst, db) }
	srcDB, err := conn(p.InstanceID, p.DbName)
	if err != nil {
		return run, err
	}
	defer srcDB.Close(context.Background())
	dstAdmin, err := conn(pt.InstanceID, "postgres")
	if err != nil {
		return run, err
	}
	defer dstAdmin.Close(context.Background())

	rep, err := logical.Preflight(ctx, srcDB, dstAdmin)
	if err != nil {
		return run, err
	}
	run.mode = ModeLogical
	var fallback *string
	if !rep.OK() {
		reason := rep.Reason()
		if !allowDump {
			return run, jobs.Permanent(fmt.Errorf("a logical move isn't possible: %s", reason))
		}
		run.mode, fallback = ModeDump, &reason
		if err := log.Warn(ctx, "preflight", "copying with dump/restore during the write freeze instead of logical replication: %s", reason); err != nil {
			return run, err
		}
	} else if err := log.Info(ctx, "preflight", "logical replication: %s, Postgres %d → %d%s",
		human(rep.SizeBytes), rep.SourceVersion/10000, rep.TargetVersion/10000, identityNote(rep.NoIdentity)); err != nil {
		return run, err
	}
	q := store.New(s.db)
	if _, err := q.StartMove(ctx, store.StartMoveParams{OperationID: op.ID, ProjectID: p.ID, SourceInstance: p.InstanceID,
		TargetInstance: pt.InstanceID, Mode: run.mode, FallbackReason: fallback}); err != nil {
		return run, err
	}
	phase := func(ph string, st *logical.Status) {
		args := store.MovePhaseParams{OperationID: op.ID, Phase: ph}
		if st != nil {
			total, ready, lag := int32(st.Tables), int32(st.Ready), st.LagBytes
			args.TablesTotal, args.TablesReady, args.LagBytes = &total, &ready, &lag
		}
		if err := q.MovePhase(ctx, args); err != nil {
			s.log.Warn("could not record move progress", "operation", op.ID, "err", err)
		}
	}

	if run.mode == ModeDump {
		if err := s.freeze(ctx, p, log); err != nil {
			return run, err
		}
		run.frozeAt = time.Now()
		phase("copying", nil)
		if err := s.afterFreeze(ctx); err != nil {
			return run, err
		}
		took, err := s.copyKeepingOwners(ctx, agent, p, srcDB, pt.InstanceID, false)
		if err != nil {
			return run, jobs.Permanent(fmt.Errorf("copy: %w", err))
		}
		if err := log.Info(ctx, "copy", "pg_dump | pg_restore on %s in %s", agent.Node.Name, took.Round(time.Millisecond)); err != nil {
			return run, err
		}
		dstDB, err := conn(pt.InstanceID, p.DbName)
		if err != nil {
			return run, err
		}
		defer dstDB.Close(context.Background())
		v, err := pgverify.Compare(ctx, srcDB, dstDB, nil)
		if err != nil {
			return run, fmt.Errorf("verify: %w", err)
		}
		if !v.OK() {
			return run, jobs.Permanent(fmt.Errorf("verification failed: %s", v.Summary()))
		}
		phase("cutover", nil)
		return run, log.Info(ctx, "verify", "verified: %d table(s), %d row(s), and %d sequence(s) match", v.Tables, v.Rows, v.Sequences)
	}

	// Logical: the schema, then replication, while the source keeps serving.
	took, err := s.copyKeepingOwners(ctx, agent, p, srcDB, pt.InstanceID, true)
	if err != nil {
		return run, jobs.Permanent(fmt.Errorf("schema copy: %w", err))
	}
	if err := log.Info(ctx, "schema", "schema copied in %s; schema changes are paused on the project until the move finishes", took.Round(time.Millisecond)); err != nil {
		return run, err
	}
	srcAdmin, err := conn(p.InstanceID, "postgres")
	if err != nil {
		return run, err
	}
	defer srcAdmin.Close(context.Background())
	dstDB, err := conn(pt.InstanceID, p.DbName)
	if err != nil {
		return run, err
	}
	defer dstDB.Close(context.Background())
	at, err := s.projects.AgentConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return run, err
	}
	m := logical.Move{Name: logical.Name(op.ID.String()), Database: p.DbName, SameMajor: rep.SourceVersion/10000 == rep.TargetVersion/10000}
	if err := logical.Setup(ctx, m, srcDB, srcAdmin, dstDB, logical.Endpoint{Host: at.Host, Port: at.Port, SSLMode: at.SSLMode}, rep.NoIdentity); err != nil {
		return run, err
	}
	phase("copying", nil)
	if err := log.Info(ctx, "replicate", "publication and subscription created; Postgres is copying the data"); err != nil {
		return run, err
	}
	var lastLog time.Time
	var lastReady int
	report := func(st logical.Status) error {
		ph := "copying"
		if st.Caught() {
			ph = "streaming"
		}
		phase(ph, &st)
		if st.Ready != lastReady || time.Since(lastLog) > 30*time.Second {
			lastLog, lastReady = time.Now(), st.Ready
			return log.Info(ctx, "replicate", "%d of %d table(s) copied, %s behind", st.Ready, st.Tables, human(st.LagBytes))
		}
		return nil
	}
	if err := logical.WaitCaughtUp(ctx, m, srcDB, dstDB, s.cfg.MoveWait.withReport(report)); err != nil {
		return run, fmt.Errorf("replication: %w", err)
	}

	// The cutover: a few seconds of frozen writes, whatever the size.
	if err := s.freeze(ctx, p, log); err != nil {
		return run, err
	}
	run.frozeAt = time.Now()
	phase("cutover", nil)
	if err := s.afterFreeze(ctx); err != nil {
		return run, err
	}
	res, err := logical.Cutover(ctx, m, srcDB, dstDB, cutoverTimeout)
	if err != nil {
		return run, jobs.Permanent(fmt.Errorf("cutover: %w", err))
	}
	return run, log.Info(ctx, "verify", "target caught up in %s; %d sequence(s) copied with a margin of %d; %d sampled table(s) match",
		res.SyncWait.Round(time.Millisecond), res.Sequences, logical.Margin, len(res.Verified))
}

func (s *Service) afterFreeze(ctx context.Context) error {
	if s.cfg.AfterFreeze != nil {
		if err := s.cfg.AfterFreeze(ctx); err != nil {
			return jobs.Permanent(err)
		}
	}
	return nil
}

func identityNote(tables []string) string {
	if len(tables) == 0 {
		return ""
	}
	return fmt.Sprintf("; %s without a primary key get REPLICA IDENTITY FULL (updates replicate more slowly)", plural(len(tables), "table", "tables"))
}

// cleanupMove removes a logical move's replication objects from both
// instances, best effort (a dump/restore move has none).
func (s *Service) cleanupMove(ctx context.Context, op store.Operation, source, target uuid.UUID, db string, log *jobs.StepLogger) {
	m := logical.Move{Name: logical.Name(op.ID.String()), Database: db}
	open := func(inst uuid.UUID, name string) *pgx.Conn {
		c, err := s.projects.AdminConn(ctx, inst, name)
		if err != nil {
			return nil
		}
		return c
	}
	srcDB, srcAdmin, dstDB := open(source, db), open(source, "postgres"), open(target, db)
	for _, c := range []*pgx.Conn{srcDB, srcAdmin, dstDB} {
		if c != nil {
			defer c.Close(context.Background())
		}
	}
	if srcAdmin == nil {
		srcDB = nil
	}
	if err := logical.Cleanup(ctx, m, srcDB, srcAdmin, dstDB); err != nil && log != nil {
		_ = log.Warn(ctx, "cleanup", "could not remove every replication object: %v", err)
	}
}

// finishMove re-renders the route, resumes the poolers, removes the
// replication objects, keeps the source (read-only or stopped), and takes
// the first backup on the target. It is idempotent.
func (s *Service) finishMove(ctx context.Context, op store.Operation, p store.Project, source uuid.UUID, log *jobs.StepLogger, frozeAt time.Time) error {
	// After the cutover the replication objects only cost the source WAL:
	// remove them whatever happens next (a slot left behind keeps WAL
	// forever).
	defer s.cleanupMove(context.WithoutCancel(ctx), op, source, p.InstanceID, p.DbName, log)
	if err := s.projects.SyncPooler(ctx, log, "pooler", "route switched to the new instance"); err != nil {
		return err
	}
	if err := s.projects.Pooler().Resume(ctx, store.PoolerNames(p)...); err != nil && !isNotPaused(err) {
		return fmt.Errorf("pooler RESUME: %w", err)
	}
	q := store.New(s.db)
	if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: provision.StatusActive}); err != nil {
		return err
	}
	msg := "resumed; clients reach the new instance with the same URL" + s.moveDone(ctx, op, frozeAt)
	if err := log.Info(ctx, "pooler", "%s", msg); err != nil {
		return err
	}
	if err := s.retireSource(ctx, p, log); err != nil {
		return err
	}
	if params, err := opMove(op); err == nil && params.Resized {
		var from, to int32
		if err := s.db.QueryRow(ctx, `SELECT coalesce((SELECT volume_gb FROM instances WHERE id = $1), 0), coalesce((SELECT volume_gb FROM instances WHERE id = $2), 0)`,
			source, p.InstanceID).Scan(&from, &to); err == nil && from != to {
			if err := s.followDiskWarn(ctx, p.ID, int(from), int(to)); err != nil {
				return err
			}
		}
		if err := log.Info(ctx, "resize", "the project runs at its new size on the new node"); err != nil {
			return err
		}
	}
	if p.Tier == provision.TierDedicated {
		s.projects.Provisioned(ctx, p, log)
	} else if s.Snapshot != nil {
		if err := s.Snapshot(ctx, p, log); err != nil {
			_ = log.Warn(ctx, "backup", "first backup on the new node failed: %v (the nightly schedule will take one)", err)
		}
	}
	if op.Kind == KindUpgrade {
		var v int32
		if err := s.db.QueryRow(ctx, `SELECT pg_version FROM instances WHERE id = $1`, p.InstanceID).Scan(&v); err == nil {
			return log.Info(ctx, "done", "project upgraded to Postgres %d", v)
		}
	}
	return log.Info(ctx, "done", "project moved")
}

// movingStatus is the project status a move operation runs under.
func movingStatus(op store.Operation) string {
	if op.Kind == KindUpgrade {
		return provision.StatusUpgrading
	}
	return provision.StatusMoving
}

// retireSource leaves what a move kept: a shared copy read-only, a
// dedicated instance stopped. Both go when the retention ends (DropRetired).
func (s *Service) retireSource(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	q := store.New(s.db)
	retired, err := q.ListLiveRetiredForProject(ctx, p.ID)
	if err != nil {
		return err
	}
	for _, r := range retired {
		switch r.Reason {
		case retiredMoveCopy:
			conn, err := s.projects.AdminConn(ctx, r.InstanceID, "postgres")
			if err != nil {
				return err
			}
			_, err = conn.Exec(ctx, "ALTER DATABASE "+provision.Ident(r.DbName)+" SET default_transaction_read_only = on")
			_ = conn.Close(context.Background())
			if err != nil {
				return err
			}
			if err := log.Info(ctx, "source", "old copy kept read-only until %s, then dropped", r.DropAfter.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		case retiredMoveInstance:
			inst, err := q.GetInstance(ctx, r.InstanceID)
			if err != nil {
				return err
			}
			if inst.Status == "running" {
				agent, err := s.nodes.ForNode(ctx, inst.NodeID)
				if err != nil {
					return err
				}
				if _, err := agent.StopInstance(ctx, agentKey(inst)); err != nil {
					return fmt.Errorf("stop the old instance: %w", err)
				}
				if err := q.SetInstanceStatus(ctx, store.SetInstanceStatusParams{ID: inst.ID, Status: "stopped"}); err != nil {
					return err
				}
			}
			if err := log.Info(ctx, "source", "old instance stopped; its volume is kept until %s, then destroyed", r.DropAfter.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		}
	}
	return nil
}

// failMove is the rollback: before the cutover the source serves again and
// the target goes; after it, only the finishing steps remain.
func (s *Service) failMove(ctx context.Context, op store.Operation, log *jobs.StepLogger, _ error) error {
	params, err := opMove(op)
	if err != nil {
		return err
	}
	q := store.New(s.db)
	p, err := q.GetProject(ctx, *op.ProjectID)
	if err != nil {
		return err
	}
	if p.InstanceID == params.TargetInstance {
		return s.finishMove(ctx, op, p, params.SourceInstance, log, time.Time{})
	}
	s.moveFailed(ctx, op, p.InstanceID, params.TargetInstance, p.DbName, log)
	if err := s.unfreeze(ctx, p, log, "source writable again; route resumed"); err != nil {
		return err
	}
	if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: provision.StatusActive}); err != nil {
		return err
	}
	if params.NewInstance {
		if inst, err := q.GetInstance(ctx, params.TargetInstance); err == nil {
			if err := s.destroyInstance(ctx, inst, log); err != nil {
				_ = log.Warn(ctx, "rollback", "could not remove the new instance: %v", err)
			}
		}
	} else if err := s.dropCopy(ctx, params.TargetInstance, p.DbName, p.OwnerRole); err != nil {
		_ = log.Warn(ctx, "rollback", "could not drop the partial copy: %v", err)
	}
	return log.Warn(ctx, "rollback", "move rolled back; the project stays where it was with no data lost")
}

// MoveWait tunes how long a logical move waits before cutting over.
type MoveWait struct {
	MaxLag    int64
	StableFor time.Duration
	Poll      time.Duration
}

func (w MoveWait) withReport(r func(logical.Status) error) logical.WaitOptions {
	return logical.WaitOptions{MaxLag: w.MaxLag, StableFor: w.StableFor, Poll: w.Poll, Report: r}
}

// moveDone records a finished move (its pause, if this attempt froze
// writes) and returns the pause for the log.
func (s *Service) moveDone(ctx context.Context, op store.Operation, frozeAt time.Time) string {
	args := store.MovePhaseParams{OperationID: op.ID, Phase: "done"}
	msg := ""
	if !frozeAt.IsZero() {
		froze := time.Since(frozeAt)
		ms := int32(froze.Milliseconds())
		args.FreezeMs = &ms
		msg = fmt.Sprintf(" (writes were paused for %s)", froze.Round(10*time.Millisecond))
	}
	if err := store.New(s.db).MovePhase(ctx, args); err != nil {
		s.log.Warn("could not record move progress", "operation", op.ID, "err", err)
	}
	return msg
}

// moveFailed records a move rolled back before its cutover and removes its
// replication objects.
func (s *Service) moveFailed(ctx context.Context, op store.Operation, source, target uuid.UUID, db string, log *jobs.StepLogger) {
	if err := store.New(s.db).MovePhase(ctx, store.MovePhaseParams{OperationID: op.ID, Phase: "failed"}); err != nil {
		s.log.Warn("could not record move progress", "operation", op.ID, "err", err)
	}
	s.cleanupMove(ctx, op, source, target, db, log)
}
