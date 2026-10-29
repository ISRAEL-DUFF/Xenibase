package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
)

// Branches of a project with backend services (V4.1 §9.5): the branch gets
// its own API: a new ref, new keys, a new signing key (never the parent's,
// so neither's tokens work on the other), the parent's API and auth
// settings without their secrets, and the users the data copy brought.

// KindCopyBranchFiles copies a parent's stored files into a branch's.
const KindCopyBranchFiles = "copy_branch_files"

// BranchAPI is a new branch's API, returned once with its keys.
type BranchAPI struct {
	Ref  string
	URL  string
	Keys []CreatedKey
}

// PrepareBranch gives branch an API of its own when parent has backend
// services: a new ref and keys, and the parent's settings (exposed
// schemas, public tables, CORS origins, auth configuration and templates)
// without its secrets (SMTP, SMS and OAuth credentials, the captcha and
// hook secrets), which the branch's owner sets. It returns nil when the
// parent has none; the branch's create operation turns them on.
func (s *Service) PrepareBranch(ctx context.Context, parentID, branchID uuid.UUID, by *uuid.UUID) (*BranchAPI, error) {
	var out *BranchAPI
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		parent, err := q.GetProjectServices(ctx, parentID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !parent.Enabled) {
			return nil
		}
		if err != nil {
			return err
		}
		svc, err := ensureRow(ctx, q, branchID)
		if err != nil {
			return err
		}
		if err := q.CopyServicesSettings(ctx, store.CopyServicesSettingsParams{Branch: branchID, Parent: parentID}); err != nil {
			return err
		}
		if err := q.CopyAuthConfig(ctx, store.CopyAuthConfigParams{Branch: branchID, Parent: parentID}); err != nil {
			return err
		}
		out = &BranchAPI{Ref: svc.Ref}
		for _, kind := range []string{KindPublishable, KindSecret} {
			k, err := insertKey(ctx, q, branchID, kind, "default", by)
			if err != nil {
				return err
			}
			out.Keys = append(out.Keys, k)
		}
		return nil
	})
	if err != nil || out == nil {
		return nil, err
	}
	if b, err := store.New(s.db).GetProject(ctx, branchID); err == nil {
		out.URL = s.URL(out.Ref, b.Region)
	}
	return out, nil
}

// EnableBranch turns on the API PrepareBranch made, once the branch holds
// its copy of the parent: the roles, xb_* schemas and a signing key of its
// own. The parent's sessions came with the copy and are dropped, so a
// parent's refresh token can't be used here. With copyFiles, the parent's
// stored files are copied in the background. A branch without a prepared
// API (its parent had none) is left alone; after a reset it runs again.
func (s *Service) EnableBranch(ctx context.Context, branch store.Project, parentID uuid.UUID, copyFiles bool, log *jobs.StepLogger) error {
	svc, err := store.New(s.db).GetProjectServices(ctx, branch.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.purgeSessions(ctx, branch, log); err != nil {
		return err
	}
	if err := s.enableOn(ctx, branch, log); err != nil {
		return err
	}
	if !copyFiles {
		return nil
	}
	op, err := jobs.Enqueue(ctx, s.db, jobs.EnqueueParams{Kind: KindCopyBranchFiles, ProjectID: &branch.ID, Background: true,
		Params: map[string]any{"parent": parentID}})
	if err != nil {
		return err
	}
	return log.Info(ctx, "files", "copying %s's files to %s in the background (operation %s)", parentID, svc.Ref, op.ID)
}

// purgeSessions drops the sessions and refresh tokens a copy brought.
func (s *Service) purgeSessions(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var has bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('xb_auth.sessions') IS NOT NULL`).Scan(&has); err != nil || !has {
		return err
	}
	tag, err := conn.Exec(ctx, `DELETE FROM xb_auth.sessions`) // refresh tokens and MFA challenges go with them
	if err != nil {
		return err
	}
	for _, t := range []string{"xb_auth.refresh_tokens", "xb_auth.flow_state", "xb_auth.one_time_codes", "xb_auth.mfa_challenges"} {
		if _, err := conn.Exec(ctx, `DELETE FROM `+t); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
	}
	return log.Info(ctx, "sessions", "the parent's %d sessions were dropped from the copy; its users sign in again here", tag.RowsAffected())
}

func (s *Service) runCopyBranchFiles(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	var params struct {
		Parent uuid.UUID `json:"parent"`
	}
	if err := json.Unmarshal(op.Params, &params); err != nil || params.Parent == uuid.Nil {
		return jobs.Permanent(fmt.Errorf("copy branch files: no parent in the params (%w)", err))
	}
	q := store.New(s.db)
	branch, err := q.GetProject(ctx, *op.ProjectID)
	if err != nil {
		return jobs.Permanent(err)
	}
	from, err := q.GetProjectServices(ctx, params.Parent)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("the parent's services: %w", err))
	}
	to, err := q.GetProjectServices(ctx, branch.ID)
	if err != nil {
		return jobs.Permanent(err)
	}
	cl, err := s.filesClient(ctx, branch.Region, branch.DataResidency)
	if err != nil {
		return err
	}
	conn, err := s.projects.AdminConn(ctx, branch.InstanceID, branch.DbName)
	if err != nil {
		return err
	}
	rows, err := conn.Query(ctx, `SELECT version FROM xb_storage.objects ORDER BY created_at`)
	if err != nil {
		conn.Close(context.Background())
		return err
	}
	var versions []uuid.UUID
	for rows.Next() {
		var v uuid.UUID
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			conn.Close(context.Background())
			return err
		}
		versions = append(versions, v)
	}
	rows.Close()
	conn.Close(context.Background())
	copied, missing := 0, 0
	start := time.Now()
	for _, v := range versions {
		dst := objectsKey(to.Ref, v)
		if _, _, err := cl.Head(ctx, dst); err == nil {
			continue // a retry: already there
		}
		if _, err := cl.Copy(ctx, objectsKey(from.Ref, v), dst); err != nil {
			missing++ // gone from the parent since the copy, or never stored
			continue
		}
		copied++
	}
	return log.Info(ctx, "files", "copied %d files in %s (%d not found in the parent)", copied, time.Since(start).Round(time.Millisecond), missing)
}
