package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Entry points for the API layer.

// Config returns the effective configuration.
func (s *Service) Config() Config { return s.cfg }

// ready checks that backups can run at all.
func (s *Service) ready(ctx context.Context) error {
	if _, _, err := s.StorageTarget(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	if _, err := s.BackupKey(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return nil
}

// BackupNow queues a logical backup of a project.
func (s *Service) BackupNow(ctx context.Context, projectID uuid.UUID, by *uuid.UUID) (store.Operation, error) {
	if err := s.ready(ctx); err != nil {
		return store.Operation{}, err
	}
	p, err := s.projects.Get(ctx, projectID)
	if err != nil {
		return store.Operation{}, err
	}
	if p.Tier == provision.TierDedicated {
		return s.projects.EnqueueExclusive(ctx, projectID, []string{provision.StatusActive}, "", KindBaseBackup,
			map[string]any{}, by, nil)
	}
	return s.projects.EnqueueExclusive(ctx, projectID, []string{provision.StatusActive}, "", KindBackup,
		backupParams{Kind: Logical}, by, nil)
}

// PITRParams asks for a point-in-time recovery of a dedicated project into
// a new one.
type PITRParams struct {
	ProjectID uuid.UUID
	// TargetTime is the moment to recover to; nil is the latest.
	TargetTime *time.Time
	Name       string
	CreatedBy  *uuid.UUID
	// CreatorRole makes CreatedBy a member of the new project (see
	// provision.CreateParams).
	CreatorRole string
}

// PITR queues a point-in-time recovery into a new dedicated project on the
// source's node with the same size.
func (s *Service) PITR(ctx context.Context, p PITRParams) (provision.Created, error) {
	if s.Dedicated == nil {
		return provision.Created{}, provision.ErrNoDedicated
	}
	if err := s.ready(ctx); err != nil {
		return provision.Created{}, err
	}
	src, err := s.projects.Get(ctx, p.ProjectID)
	if err != nil {
		return provision.Created{}, err
	}
	if strings.TrimSpace(p.Name) == "" {
		return provision.Created{}, fmt.Errorf("%w: name the new project", ErrInvalid)
	}
	plan, err := s.Dedicated.PlanPITR(ctx, src, p.TargetTime)
	if err != nil {
		return provision.Created{}, err
	}
	inst, err := store.New(s.db).GetInstance(ctx, src.InstanceID)
	if err != nil {
		return provision.Created{}, err
	}
	// The base backup's major, which before a major upgrade is not the
	// project's now: its data won't start on another.
	major, err := s.Dedicated.SourceMajor(ctx, plan)
	if err != nil {
		return provision.Created{}, err
	}
	cp := provision.CreateParams{
		OrgID: src.OrgID, CreatorRole: p.CreatorRole,
		Name: strings.TrimSpace(p.Name), CreatedBy: p.CreatedBy, Kind: KindRestore,
		Tier: provision.TierDedicated, NodeID: &inst.NodeID, PgVersion: major, Derived: true,
		Region: src.Region, DataResidency: src.DataResidency, // a restore stays in the source's region
		Params: map[string]any{"mode": ModePITR, "pitr": plan},
	}
	// The source's size, a profile's or a resize's ("custom").
	cp.Size = dedicated.SizeProfile(inst)
	if inst.VolumeGb != nil {
		cp.VolumeGB = int(*inst.VolumeGb)
	}
	cp.MinMaxConnections = int(inst.MaxConnections)
	return s.projects.Create(ctx, cp)
}

// RestoreParams describes a restore request.
type RestoreParams struct {
	BackupID uuid.UUID
	Mode     string
	// Name of the new project (ModeNew).
	Name string
	// Confirm must equal the project's name (ModeInPlace).
	Confirm     string
	CreatedBy   *uuid.UUID
	CreatorRole string
}

// Restore queues a restore. For ModeNew it also returns the new project's
// one-time credentials.
func (s *Service) Restore(ctx context.Context, p RestoreParams) (store.Operation, *provision.Created, error) {
	if err := s.ready(ctx); err != nil {
		return store.Operation{}, nil, err
	}
	b, err := store.New(s.db).GetBackup(ctx, p.BackupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Operation{}, nil, ErrNotFound
	}
	if err != nil {
		return store.Operation{}, nil, err
	}
	if b.Status != "succeeded" || b.ProjectID == nil || b.Kind == Metadata {
		return store.Operation{}, nil, fmt.Errorf("%w: only succeeded project backups can be restored", ErrInvalid)
	}
	if b.Kind == "base" {
		// A base backup restores as a recovery to the moment it finished.
		if p.Mode == ModeInPlace {
			return store.Operation{}, nil, fmt.Errorf("%w: dedicated projects restore into a new project", ErrInvalid)
		}
		c, err := s.PITR(ctx, PITRParams{ProjectID: *b.ProjectID, TargetTime: b.FinishedAt, Name: p.Name, CreatedBy: p.CreatedBy, CreatorRole: p.CreatorRole})
		if err != nil {
			return store.Operation{}, nil, err
		}
		return c.Operation, &c, nil
	}
	params := restoreParams{Mode: p.Mode, BackupID: b.ID}
	switch p.Mode {
	case "", ModeNew:
		params.Mode = ModeNew
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return store.Operation{}, nil, fmt.Errorf("%w: name the new project", ErrInvalid)
		}
		// The source may be deleted: final backups outlive their project.
		src, err := store.New(s.db).GetProject(ctx, *b.ProjectID)
		if err != nil {
			return store.Operation{}, nil, err
		}
		c, err := s.projects.Create(ctx, provision.CreateParams{
			OrgID: src.OrgID, CreatorRole: p.CreatorRole,
			Name: name, CreatedBy: p.CreatedBy, Kind: KindRestore,
			Region: src.Region, DataResidency: src.DataResidency, // a restore stays in the source's region
			Params: map[string]any{"mode": params.Mode, "backup_id": params.BackupID},
		})
		if err != nil {
			return store.Operation{}, nil, err
		}
		return c.Operation, &c, nil
	case ModeInPlace:
		op, err := s.projects.EnqueueExclusive(ctx, *b.ProjectID, []string{provision.StatusActive}, provision.StatusRestoring,
			KindRestore, params, p.CreatedBy, func(pr store.Project) error {
				if p.Confirm != pr.Name {
					return fmt.Errorf("%w: confirm must match the project name exactly", provision.ErrInvalid)
				}
				return nil
			})
		return op, nil, err
	default:
		return store.Operation{}, nil, fmt.Errorf("%w: mode must be new or in_place", ErrInvalid)
	}
}

// TestRestoreNow queues a restore test, of projectID or a random project.
func (s *Service) TestRestoreNow(ctx context.Context, projectID *uuid.UUID, by *uuid.UUID) (store.Operation, error) {
	if err := s.ready(ctx); err != nil {
		return store.Operation{}, err
	}
	if projectID != nil {
		if _, err := s.projects.Get(ctx, *projectID); err != nil {
			return store.Operation{}, err
		}
	}
	return jobs.Enqueue(ctx, s.db, jobs.EnqueueParams{Kind: KindRestoreTest, ProjectID: projectID, CreatedBy: by, Background: true})
}

// Overview summarises backup health.
type Overview struct {
	StorageConfigured  bool
	Key                KeyInfo
	AgentAvailable     bool
	LastRestoreTest    *store.Operation
	LastMetadataBackup *store.Backup
}

// Overview reports backup configuration and the latest checks.
func (s *Service) Overview(ctx context.Context) (Overview, error) {
	var o Overview
	_, _, err := s.StorageTarget(ctx)
	o.StorageConfigured = err == nil
	if o.Key, err = s.KeyInfo(ctx); err != nil {
		return o, err
	}
	_, err = s.nodes.Any(ctx)
	o.AgentAvailable = err == nil
	q := store.New(s.db)
	if op, err := q.LastOperationOfKind(ctx, KindRestoreTest); err == nil {
		o.LastRestoreTest = &op
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return o, err
	}
	if rows, err := q.RetentionCandidates(ctx, store.RetentionCandidatesParams{Kind: Metadata}); err == nil && len(rows) > 0 {
		o.LastMetadataBackup = &rows[0]
	} else if err != nil {
		return o, err
	}
	return o, nil
}
