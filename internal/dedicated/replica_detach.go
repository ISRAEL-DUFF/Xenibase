package dedicated

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Detaching a read replica (V4 §7) promotes it into a standalone dedicated
// project, for analytics or a migration: it leaves the source's cluster,
// restarts as a plain instance on its own data (its instance keeps the
// member's id, which names the container and volume), is promoted, and
// takes the new project's database name, owner and password, as a
// point-in-time recovery does. The source keeps running throughout.

// DetachParams asks for a replica to become a project.
type DetachParams struct {
	ProjectID   uuid.UUID
	ReplicaID   uuid.UUID
	Name        string
	CreatedBy   *uuid.UUID
	CreatorRole string
}

// detachParams is the "detach" entry of a detach_replica operation.
type detachParams struct {
	SourceProject  uuid.UUID `json:"source_project"`
	SourceInstance uuid.UUID `json:"source_instance"`
	Replica        uuid.UUID `json:"replica"`
}

func opDetach(op store.Operation) (*detachParams, error) {
	var p struct {
		Detach *detachParams `json:"detach"`
	}
	if len(op.Params) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(op.Params, &p); err != nil {
		return nil, jobs.Permanent(err)
	}
	return p.Detach, nil
}

// DetachReplica checks and queues turning a replica into a new project,
// returned with its one-time credentials.
func (s *Service) DetachReplica(ctx context.Context, d DetachParams) (provision.Created, error) {
	name := strings.TrimSpace(d.Name)
	if name == "" {
		return provision.Created{}, fmt.Errorf("%w: name the new project", provision.ErrInvalid)
	}
	q := store.New(s.db)
	src, err := q.GetProject(ctx, d.ProjectID)
	if err != nil {
		return provision.Created{}, err
	}
	if src.Status != provision.StatusActive {
		return provision.Created{}, fmt.Errorf("%w: the project is %s", provision.ErrConflict, src.Status)
	}
	r, err := q.GetReadReplica(ctx, d.ReplicaID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (r.ProjectID != src.ID || r.DeletedAt != nil)) {
		return provision.Created{}, provision.ErrNotFound
	}
	if err != nil {
		return provision.Created{}, err
	}
	inst, err := q.GetInstance(ctx, src.InstanceID)
	if err != nil {
		return provision.Created{}, err
	}
	node, err := q.GetNode(ctx, r.NodeID)
	if err != nil {
		return provision.Created{}, err
	}
	n, err := q.MarkReplicaDetaching(ctx, store.MarkReplicaDetachingParams{ID: r.ID, ProjectID: src.ID})
	if err != nil {
		return provision.Created{}, err
	}
	if n == 0 {
		return provision.Created{}, fmt.Errorf("%w: the replica is %s; it can be detached once it streams", provision.ErrConflict, r.Status)
	}
	// Out of the read route now: it stops following the primary soon.
	if err := s.projects.SyncPooler(ctx, nil, "pooler", "read replica detaching"); err != nil {
		_ = q.UnmarkReplicaDetaching(context.WithoutCancel(ctx), r.ID)
		return provision.Created{}, err
	}
	cp := provision.CreateParams{
		OrgID: src.OrgID, CreatorRole: d.CreatorRole, Name: name, CreatedBy: d.CreatedBy, Kind: KindDetachReplica,
		Tier: provision.TierDedicated, NodeID: &r.NodeID, PgVersion: int(inst.PgVersion), InstanceID: &r.ID,
		Region: node.Region, DataResidency: src.DataResidency && node.Region == src.Region,
		Params: map[string]any{"detach": detachParams{SourceProject: src.ID, SourceInstance: inst.ID, Replica: r.ID}},
	}
	if inst.Profile != nil {
		cp.Profile = *inst.Profile
	}
	if inst.VolumeGb != nil {
		cp.VolumeGB = int(*inst.VolumeGb)
	}
	c, err := s.projects.Create(ctx, cp)
	if err != nil {
		_ = q.UnmarkReplicaDetaching(context.WithoutCancel(ctx), r.ID)
		_ = s.projects.SyncPooler(context.WithoutCancel(ctx), nil, "pooler", "read replica stays")
		return provision.Created{}, err
	}
	return c, nil
}

// runDetachReplica builds the new project on the replica's data.
func (s *Service) runDetachReplica(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	password, err := s.projects.Password(op)
	if err != nil {
		return err
	}
	if err := s.projects.EnsureInstance(ctx, op, p, log); err != nil {
		return err
	}
	if err := s.projects.Prepare(ctx, p, log); err != nil {
		return err
	}
	if err := s.projects.Publish(ctx, p, password, log); err != nil {
		return err
	}
	return log.Info(ctx, "done", "read replica detached as project %s", p.Name)
}

// failDetachReplica: the new project goes as any failed create does,
// taking the replica's container with it (it has left the source's
// cluster by then, or never will).
func (s *Service) failDetachReplica(ctx context.Context, op store.Operation, log *jobs.StepLogger, cause error) error {
	d, err := opDetach(op)
	if err != nil {
		return err
	}
	if err := s.projects.Rollback(ctx, op, log, cause); err != nil {
		return err
	}
	if d != nil {
		q := store.New(s.db)
		if err := q.DeleteInstanceMember(ctx, d.Replica); err != nil {
			return err
		}
		if err := q.DeleteReadReplica(ctx, d.Replica); err != nil {
			return err
		}
		_ = s.projects.SyncPooler(ctx, log, "pooler", "read replica removed")
	}
	return log.Warn(ctx, "rollback", "the replica was not detached and has been removed: %v. The source project is unaffected; create a new replica if you need one.", cause)
}

// ensureDetached is Ensure for a detached replica: the member leaves the
// source's cluster and its container restarts as a plain instance on the
// same volume, which is then promoted.
func (s *Service) ensureDetached(ctx context.Context, inst store.Instance, p store.Project, d *detachParams, log *jobs.StepLogger) error {
	q := store.New(s.db)
	if inst.Status != "running" {
		if len(inst.AdminSecret) == 0 {
			// The superuser is the source's: it is in the replica's data.
			src, err := q.GetInstance(ctx, d.SourceInstance)
			if err != nil {
				return jobs.Permanent(fmt.Errorf("source instance: %w", err))
			}
			secret, err := provision.OpenInstanceSecret(s.keyring, src.ID, src.AdminSecret)
			if err != nil {
				return jobs.Permanent(err)
			}
			sealed, err := provision.SealInstanceSecret(s.keyring, inst.ID, secret)
			if err != nil {
				return err
			}
			if err := q.SetInstanceAdminSecret(ctx, store.SetInstanceAdminSecretParams{ID: inst.ID, AdminSecret: sealed}); err != nil {
				return err
			}
			inst.AdminSecret = sealed
		}
		var err error
		if inst, err = s.placeArchive(ctx, inst, p); err != nil {
			return err
		}
		// Out of the source's cluster: no member row, so neither the
		// watcher nor HA counts it.
		if err := q.DeleteInstanceMember(ctx, d.Replica); err != nil {
			return err
		}
		spec, err := s.instanceSpec(ctx, inst)
		if err != nil {
			return err
		}
		spec.Recreate = true
		agent, err := s.nodes.ForNode(ctx, inst.NodeID)
		if err != nil {
			return err
		}
		if err := log.Info(ctx, "detach", "restarting the replica on %s as a standalone instance", agent.Node.Name); err != nil {
			return err
		}
		res, err := agent.CreateInstance(ctx, spec)
		if err != nil {
			return fmt.Errorf("restart the replica: %w", err)
		}
		if err := s.recordRunning(ctx, inst, agent, res); err != nil {
			return err
		}
		if inst, err = q.GetInstance(ctx, inst.ID); err != nil {
			return err
		}
	}
	conn, err := s.waitConn(ctx, inst)
	if err != nil {
		return err
	}
	var inRecovery bool
	err = conn.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery)
	if err == nil && inRecovery {
		var promoted bool
		err = conn.QueryRow(ctx, `SELECT pg_promote(true, 120)`).Scan(&promoted)
		if err == nil && !promoted {
			err = errors.New("the replica did not finish promoting in 2 minutes")
		}
	}
	if err == nil {
		// A promoted former standby still names the source's primary.
		// So do the replica's standby settings, which would otherwise pass
		// to the new project's own standbys.
		for _, stmt := range []string{"ALTER SYSTEM RESET primary_conninfo", "ALTER SYSTEM RESET primary_slot_name",
			"ALTER SYSTEM RESET hot_standby_feedback", "ALTER SYSTEM RESET max_standby_streaming_delay",
			"ALTER SYSTEM RESET max_standby_archive_delay", "SELECT pg_reload_conf()"} {
			if _, err = conn.Exec(ctx, stmt); err != nil {
				break
			}
		}
	}
	conn.Close(context.Background())
	if err != nil {
		return fmt.Errorf("promote the replica: %w", err)
	}
	if err := q.SetReplicaDetached(ctx, store.SetReplicaDetachedParams{ID: d.Replica, DetachedProjectID: &p.ID}); err != nil {
		return err
	}
	if err := s.projects.SyncPooler(ctx, log, "pooler", "read replica detached"); err != nil {
		return err
	}
	if err := log.Info(ctx, "detach", "promoted at %s", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return s.adoptRestore(ctx, inst, p, &pitrParams{SourceProject: d.SourceProject, SourceInstance: d.SourceInstance}, log)
}
