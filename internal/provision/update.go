package provision

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
)

// KindApplySettings re-applies a project's guardrails to the backend role
// and the pooler.
const KindApplySettings = "apply_settings"

// SettingsPatch changes some guardrails; nil fields stay as they are.
type SettingsPatch struct {
	ConnectionLimit          *int
	PoolSize                 *int
	StatementTimeout         *string
	IdleInTransactionTimeout *string
	DiskWarnBytes            *int64
	ConsoleReadOnly          *bool
	BackupRetention          *string
}

// UpdateParams describes a project update; nil fields stay as they are. An
// empty Description clears it.
type UpdateParams struct {
	Name        *string
	Description *string
	Settings    *SettingsPatch
}

// Updated is returned by Update. Operation is set when backend or pooler
// settings changed and an apply_settings operation was queued.
type Updated struct {
	Project   store.Project
	Operation *store.Operation
}

func applyPatch(s store.ProjectSettings, p SettingsPatch) (store.ProjectSettings, bool, error) {
	backend := false
	if p.ConnectionLimit != nil {
		if *p.ConnectionLimit < 1 || *p.ConnectionLimit > 1000 {
			return s, false, fmt.Errorf("%w: connection_limit must be 1-1000", ErrInvalid)
		}
		backend = backend || *p.ConnectionLimit != s.ConnectionLimit
		s.ConnectionLimit = *p.ConnectionLimit
	}
	if p.PoolSize != nil {
		if *p.PoolSize < 1 || *p.PoolSize > 1000 {
			return s, false, fmt.Errorf("%w: pool_size must be 1-1000", ErrInvalid)
		}
		backend = backend || *p.PoolSize != s.PoolSize
		s.PoolSize = *p.PoolSize
	}
	for _, f := range []struct {
		in   *string
		dst  *string
		name string
	}{
		{p.StatementTimeout, &s.StatementTimeout, "statement_timeout"},
		{p.IdleInTransactionTimeout, &s.IdleInTransactionTimeout, "idle_in_transaction_session_timeout"},
	} {
		if f.in == nil {
			continue
		}
		if *f.in != "" && !validDuration(*f.in) {
			return s, false, fmt.Errorf("%w: %s must be a duration like 60s, 500ms, or 5min", ErrInvalid, f.name)
		}
		backend = backend || *f.in != *f.dst
		*f.dst = *f.in
	}
	if p.DiskWarnBytes != nil {
		if *p.DiskWarnBytes < 0 {
			return s, false, fmt.Errorf("%w: disk_warn_bytes must not be negative", ErrInvalid)
		}
		s.DiskWarnBytes = *p.DiskWarnBytes
	}
	if p.BackupRetention != nil {
		if _, ok := store.BackupRetentionLevel(*p.BackupRetention); !ok {
			return s, false, fmt.Errorf("%w: backup_retention must be standard, extended or long", ErrInvalid)
		}
		s.BackupRetention = *p.BackupRetention
		if s.BackupRetention == store.RetentionStandard {
			s.BackupRetention = ""
		}
	}
	if p.ConsoleReadOnly != nil {
		s.ConsoleReadOnly = *p.ConsoleReadOnly
	}
	if s.PoolSize > s.ConnectionLimit {
		return s, false, fmt.Errorf("%w: pool_size cannot exceed connection_limit", ErrInvalid)
	}
	return s, backend, nil
}

// Update changes a project's name, description, or guardrails.
func (s *Service) Update(ctx context.Context, id uuid.UUID, p UpdateParams, by *uuid.UUID) (Updated, error) {
	var out Updated
	err := s.withIdleProject(ctx, id, []string{StatusActive}, func(tx pgx.Tx, proj store.Project) error {
		name := proj.Name
		if p.Name != nil {
			n, err := ValidateName(*p.Name)
			if err != nil {
				return err
			}
			name = n // the slug and database name never change
		}
		desc := proj.Description
		if p.Description != nil {
			if len(*p.Description) > 1000 {
				return fmt.Errorf("%w: description must be at most 1000 bytes", ErrInvalid)
			}
			desc = p.Description
			if *p.Description == "" {
				desc = nil
			}
		}
		set, err := store.DecodeProjectSettings(proj.Settings)
		if err != nil {
			return err
		}
		backend := false
		if p.Settings != nil {
			if set, backend, err = applyPatch(set, *p.Settings); err != nil {
				return err
			}
			if p.Settings.ConnectionLimit != nil {
				if err := s.checkConnectionLimit(ctx, store.New(tx), proj, set.ConnectionLimit); err != nil {
					return err
				}
			}
		}
		raw, err := json.Marshal(set)
		if err != nil {
			return err
		}
		q := store.New(tx)
		updated, err := q.UpdateProjectMeta(ctx, store.UpdateProjectMetaParams{ID: id, Name: name, Description: desc, Settings: raw})
		if err != nil {
			return err
		}
		out.Project = updated
		if backend {
			op, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: KindApplySettings, ProjectID: &id, CreatedBy: by})
			if err != nil {
				return err
			}
			out.Operation = &op
		}
		return nil
	})
	return out, err
}

// runApplySettings pushes guardrails to the role and the pooler.
func (s *Service) runApplySettings(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	if p.Status != StatusActive {
		return jobs.Permanent(fmt.Errorf("project is %s, not active", p.Status))
	}
	set, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		return jobs.Permanent(err)
	}
	if err := s.applySettings(ctx, p, set, log); err != nil {
		return err
	}
	return log.Info(ctx, "done", "settings applied")
}

// applySettings pushes set to p's role and the poolers, and recycles the
// pooled server connections.
func (s *Service) applySettings(ctx context.Context, p store.Project, set store.ProjectSettings, log *jobs.StepLogger) error {
	if err := s.ensureRole(ctx, p, set, log); err != nil {
		return err
	}
	if err := s.syncPooler(ctx, log, "pooler", fmt.Sprintf("pool size %d, max %d connections", set.PoolSize, set.ConnectionLimit)); err != nil {
		return err
	}
	// Pooled server connections keep the settings they started with.
	if err := s.pooler.Reconnect(ctx, store.PoolerNames(p)...); err != nil {
		return fmt.Errorf("recycle pooled connections: %w", err)
	}
	return log.Info(ctx, "pooler", "pooled server connections recycled")
}
