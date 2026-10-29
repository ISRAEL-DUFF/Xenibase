// Package branching creates, resets, detaches, and expires database
// branches (V2 §8): shared-tier copies of a parent project, from its
// latest backup or live, that keep their URL and credentials across
// resets and delete themselves after a TTL.
package branching

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/backup"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/services"
	"github.com/israel-duff/xenibase/internal/store"
)

// Operation kinds.
const (
	KindCreate = "branch_create"
	KindReset  = "branch_reset"
)

// Sources.
const (
	SourceBackup = "backup"
	SourceLive   = "live"
)

// Lifetimes (V2 §8.2).
const (
	DefaultTTL = 7 * 24 * time.Hour
	MinTTL     = time.Hour
	MaxTTL     = 30 * 24 * time.Hour
	// warnBefore is when the creator is emailed before expiry.
	warnBefore = 24 * time.Hour
)

// Errors for the API layer.
var (
	ErrInvalid   = errors.New("invalid branch request")
	ErrConflict  = errors.New("branch conflict")
	ErrForbidden = errors.New("not allowed")
	ErrNotBranch = errors.New("the project is not a branch")
)

// Config tunes the service.
type Config struct {
	// Now is the clock for expiry (tests move it).
	Now       func() time.Time
	PublicURL string
}

// API gives branches of projects with backend services their own API
// (the services service, V4.1 §9.5).
type API interface {
	PrepareBranch(ctx context.Context, parentID, branchID uuid.UUID, by *uuid.UUID) (*services.BranchAPI, error)
	EnableBranch(ctx context.Context, branch store.Project, log *jobs.StepLogger) error
	BranchFiles(ctx context.Context, branch store.Project, parentID uuid.UUID, f services.BranchFiles, log *jobs.StepLogger) error
}

// Service manages branches.
type Service struct {
	// API, when set, gives branches backend services like their parent's.
	API      API
	db       *pgxpool.Pool
	projects *provision.Service
	backups  *backup.Service
	nodes    *nodes.Service
	mail     *mail.Service
	cfg      Config
	log      *slog.Logger
}

// New returns a Service.
func New(db *pgxpool.Pool, projects *provision.Service, backups *backup.Service, ns *nodes.Service, m *mail.Service, cfg Config, log *slog.Logger) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{db: db, projects: projects, backups: backups, nodes: ns, mail: m, cfg: cfg, log: log}
}

// Kinds returns the operation kinds this service runs.
func (s *Service) Kinds() map[string]jobs.Kind {
	return map[string]jobs.Kind{
		KindCreate: {Handler: s.runCreate, OnFail: s.projects.Rollback, MaxAttempts: 2, Timeout: 6 * time.Hour},
		KindReset:  {Handler: s.runReset, OnFail: s.failReset, MaxAttempts: 2, Timeout: 6 * time.Hour},
	}
}

// CreateParams asks for a branch.
type CreateParams struct {
	ParentID uuid.UUID
	Name     string
	// Source is SourceBackup (the default) or SourceLive.
	Source string
	// SchemaOnly defaults to whether the parent contains sensitive data.
	SchemaOnly *bool
	// TTL defaults to 7 days; 0 keeps the branch until deleted.
	TTL *time.Duration
	// MayCopySensitive is set when the caller is the parent's project
	// admin: only they may take a full copy of sensitive data (V2 §8.5).
	MayCopySensitive bool
	CreatedBy        *uuid.UUID
	CreatorRole      string
	// CopyFiles copies the parent's stored files into the branch (backend
	// services) in the background.
	CopyFiles bool
}

// fillParams are a create or reset operation's "branch" params.
type fillParams struct {
	Parent     uuid.UUID `json:"parent"`
	Source     string    `json:"source"`
	SchemaOnly bool      `json:"schema_only"`
	CopyFiles  bool      `json:"copy_files,omitempty"`
}

// Resolved is what a create request turns into, for quota checks first.
type Resolved struct {
	Parent     store.Project
	Source     string
	SchemaOnly bool
	ExpiresAt  *time.Time
}

// Resolve checks a create request against the parent and fills defaults.
func (s *Service) Resolve(ctx context.Context, p CreateParams) (Resolved, error) {
	parent, err := s.projects.Get(ctx, p.ParentID)
	if err != nil {
		return Resolved{}, err
	}
	r := Resolved{Parent: parent, Source: p.Source}
	if parent.ParentProjectID != nil {
		return r, fmt.Errorf("%w: a branch can't have branches of its own (one level deep)", ErrInvalid)
	}
	if parent.Status != provision.StatusActive {
		return r, fmt.Errorf("%w: the parent is %s", ErrConflict, parent.Status)
	}
	switch r.Source {
	case "":
		r.Source = SourceBackup
	case SourceBackup, SourceLive:
	default:
		return r, fmt.Errorf("%w: source must be backup or live", ErrInvalid)
	}
	// A sleeping Free project's database refuses connections (V3 §4); its
	// backups still branch.
	if r.Source == SourceLive && parent.Lifecycle != "" && parent.Lifecycle != "active" {
		return r, fmt.Errorf("%w: %s is %s for inactivity; resume it to branch from live, or branch from its backup", ErrConflict, parent.Name, parent.Lifecycle)
	}
	r.SchemaOnly = parent.SensitiveData
	if p.SchemaOnly != nil {
		r.SchemaOnly = *p.SchemaOnly
	}
	if !r.SchemaOnly && parent.SensitiveData && !p.MayCopySensitive {
		return r, fmt.Errorf("%w: the parent contains sensitive data; a full-data branch needs the project admin role (or take schema only)", ErrForbidden)
	}
	ttl := DefaultTTL
	if p.TTL != nil {
		ttl = *p.TTL
	}
	if ttl != 0 {
		if ttl < MinTTL || ttl > MaxTTL {
			return r, fmt.Errorf("%w: the TTL must be between 1h and 30 days (or 0 to keep it)", ErrInvalid)
		}
		t := s.cfg.Now().Add(ttl).UTC()
		r.ExpiresAt = &t
	}
	if r.Source == SourceBackup {
		if _, err := store.New(s.db).LatestSucceededBackup(ctx, &parent.ID); errors.Is(err, pgx.ErrNoRows) {
			return r, fmt.Errorf("%w: %s has no backup to branch from yet; branch from live instead", ErrConflict, parent.Name)
		} else if err != nil {
			return r, err
		}
	}
	return r, nil
}

// Create queues a branch of r.Parent (always on the shared tier) and
// returns its one-time credentials, and its API's keys when the parent has
// backend services.
func (s *Service) Create(ctx context.Context, p CreateParams, r Resolved) (provision.Created, *services.BranchAPI, error) {
	// The parent's Postgres version, so the branch behaves like it.
	parentInst, err := store.New(s.db).GetInstance(ctx, r.Parent.InstanceID)
	if err != nil {
		return provision.Created{}, nil, err
	}
	c, err := s.projects.Create(ctx, provision.CreateParams{
		PgVersion: int(parentInst.PgVersion),
		OrgID:     r.Parent.OrgID, CreatorRole: p.CreatorRole, Name: p.Name, CreatedBy: p.CreatedBy,
		Kind: KindCreate, Tier: provision.TierShared, Sensitive: r.Parent.SensitiveData, Derived: true,
		Params: map[string]any{"branch": fillParams{Parent: r.Parent.ID, Source: r.Source, SchemaOnly: r.SchemaOnly, CopyFiles: p.CopyFiles}},
		Branch: &provision.BranchSpec{ParentID: r.Parent.ID, Source: r.Source, SchemaOnly: r.SchemaOnly, ExpiresAt: r.ExpiresAt, CopyFiles: p.CopyFiles},
	})
	if err != nil || s.API == nil {
		return c, nil, err
	}
	// The create operation is queued: it turns the API on once the copy is
	// in, so the keys made here work as soon as it finishes.
	api, err := s.API.PrepareBranch(ctx, r.Parent.ID, c.Project.ID, p.CreatedBy)
	return c, api, err
}

func opFill(op store.Operation) (fillParams, error) {
	var w struct {
		Branch fillParams `json:"branch"`
	}
	if err := json.Unmarshal(op.Params, &w); err != nil || w.Branch.Parent == uuid.Nil {
		return fillParams{}, jobs.Permanent(fmt.Errorf("branch params: %w", errors.Join(err, errors.New("no parent"))))
	}
	return w.Branch, nil
}

// runCreate provisions the branch and fills it from its parent.
func (s *Service) runCreate(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	params, err := opFill(op)
	if err != nil {
		return err
	}
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	// A retry after Publish finds the branch filled, routed and serving:
	// it is never recreated under its clients, only what follows runs.
	if p.Status == provision.StatusActive {
		return s.created(ctx, p, params, log)
	}
	password, err := s.projects.Password(op)
	if err != nil {
		return err
	}
	if err := s.projects.Prepare(ctx, p, log); err != nil {
		return err
	}
	// A retry may find a partial copy: start from an empty database.
	if op.Attempts > 1 {
		if err := s.projects.RecreateDatabase(ctx, p, log); err != nil {
			return err
		}
	}
	if err := s.fill(ctx, p, params, log); err != nil {
		return err
	}
	// The API goes on before the branch is published: a failure is retried
	// on a database no one uses yet, and rolled back with the branch.
	if s.API != nil {
		if err := s.API.EnableBranch(ctx, p, log); err != nil {
			return fmt.Errorf("backend services: %w", err)
		}
	}
	if err := s.projects.Publish(ctx, p, password, log); err != nil {
		return err
	}
	return s.created(ctx, p, params, log)
}

// created finishes a published branch: its files, in the background.
func (s *Service) created(ctx context.Context, p store.Project, params fillParams, log *jobs.StepLogger) error {
	if s.API != nil {
		if err := s.API.BranchFiles(ctx, p, params.Parent, services.BranchFiles{Copy: params.CopyFiles}, log); err != nil {
			return fmt.Errorf("files: %w", err)
		}
	}
	return log.Info(ctx, "done", "branch %s ready", p.Name)
}

// fill copies the parent's extensions, then its schema (and data unless
// schema only) from its latest backup or live.
func (s *Service) fill(ctx context.Context, p store.Project, params fillParams, log *jobs.StepLogger) error {
	parent, err := store.New(s.db).GetProject(ctx, params.Parent)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("parent: %w", err))
	}
	if parent.DeletedAt != nil {
		return jobs.Permanent(fmt.Errorf("the parent %s was deleted", parent.Name))
	}
	// A branch and its parent share an organisation (V2 §8.1); one that
	// doesn't is never filled from the other organisation's data.
	if parent.OrgID != p.OrgID {
		return jobs.Permanent(fmt.Errorf("the parent %s is in another organisation", parent.Name))
	}
	if err := s.copyExtensions(ctx, parent, p, log); err != nil {
		return err
	}
	what := "schema and data"
	if params.SchemaOnly {
		what = "schema only"
	}
	switch params.Source {
	case SourceLive:
		agent, err := s.nodes.ForInstance(ctx, p.InstanceID)
		if err != nil {
			return err
		}
		from, err := s.projects.AgentConn(ctx, parent.InstanceID, parent.DbName)
		if err != nil {
			return err
		}
		to, err := s.projects.RestoreConn(ctx, p, p.DbName)
		if err != nil {
			return err
		}
		if err := log.Info(ctx, "copy", "pg_dump of %s now (%s) → pg_restore as %s on %s", parent.Name, what, p.OwnerRole, agent.Node.Name); err != nil {
			return err
		}
		res, err := agent.Copy(ctx, agentapi.CopyRequest{
			Source: from, Dump: agentapi.DumpOptions{NoOwner: true, NoACL: true, SchemaOnly: params.SchemaOnly},
			Target: to, Restore: agentapi.RestoreOptions{Role: p.OwnerRole},
		})
		if err != nil {
			return jobs.Permanent(fmt.Errorf("copy from the parent: %w", err))
		}
		if err := log.Info(ctx, "copy", "copied in %s", (time.Duration(res.DurationMS) * time.Millisecond).Round(time.Millisecond)); err != nil {
			return err
		}
		// The parent's webhooks are not copied (V2 §8.1); a branch's own
		// come back after a reset.
		return s.projects.ResetWebhooks(ctx, p, log)
	default:
		b, err := store.New(s.db).LatestSucceededBackup(ctx, &parent.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return jobs.Permanent(fmt.Errorf("%s has no backup to branch from", parent.Name))
		}
		if err != nil {
			return err
		}
		if err := log.Info(ctx, "restore", "restoring %s's backup of %s (%s)", parent.Name, b.StartedAt.UTC().Format(time.RFC3339), what); err != nil {
			return err
		}
		if err := s.backups.RestoreInto(ctx, b, p, p.DbName, params.SchemaOnly, log); err != nil {
			return err
		}
		return s.projects.ResetWebhooks(ctx, p, log)
	}
}

// copyExtensions creates the parent's extensions in the branch first (as
// the admin: the restore runs as the owner role, who may not), and records
// them on the branch.
func (s *Service) copyExtensions(ctx context.Context, parent, p store.Project, log *jobs.StepLogger) error {
	src, err := s.projects.AdminConn(ctx, parent.InstanceID, parent.DbName)
	if err != nil {
		return err
	}
	rows, err := src.Query(ctx, `SELECT e.extname, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname <> 'plpgsql' ORDER BY e.extname`)
	if err != nil {
		_ = src.Close(ctx)
		return err
	}
	type ext struct{ name, schema string }
	var exts []ext
	for rows.Next() {
		var e ext
		if err := rows.Scan(&e.name, &e.schema); err != nil {
			rows.Close()
			_ = src.Close(ctx)
			return err
		}
		exts = append(exts, e)
	}
	rows.Close()
	_ = src.Close(ctx)
	if err := rows.Err(); err != nil || len(exts) == 0 {
		return err
	}
	dst, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer dst.Close(context.Background())
	q := store.New(s.db)
	var made []string
	for _, e := range exts {
		// Extensions in a schema of their own come with the dump.
		if e.schema != "public" {
			continue
		}
		if _, err := dst.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+provision.Ident(e.name)+" WITH SCHEMA public"); err != nil {
			return fmt.Errorf("create extension %s: %w", e.name, err)
		}
		if err := q.AddProjectExtension(ctx, store.AddProjectExtensionParams{ID: p.ID, Name: e.name}); err != nil {
			return err
		}
		made = append(made, e.name)
	}
	if len(made) == 0 {
		return nil
	}
	return log.Info(ctx, "extensions", "from the parent: %s", strings.Join(made, ", "))
}

// ResetParams asks for a reset; empty Source keeps the branch's own.
type ResetParams struct {
	BranchID uuid.UUID
	Source   string
	By       *uuid.UUID
	// Actor is who asks. A reset copies the parent again, so they must
	// still be allowed to branch it, and only its project admin may refill
	// a full-data branch of a parent that contains sensitive data (V2
	// §8.5), as for a new branch.
	Actor authz.Actor
}

// Reset queues re-creating a branch's data from its parent, keeping its
// database name, URL, app password, and members' credentials (V2 §8.3).
func (s *Service) Reset(ctx context.Context, p ResetParams) (store.Operation, error) {
	return s.projects.EnqueueExclusiveTx(ctx, p.BranchID, []string{provision.StatusActive, provision.StatusError}, provision.StatusRestoring, KindReset, p.By,
		func(tx pgx.Tx, b store.Project) (any, error) {
			if b.ParentProjectID == nil {
				return nil, fmt.Errorf("%w: %w", provision.ErrInvalid, ErrNotBranch)
			}
			q := store.New(tx)
			parent, err := q.GetProject(ctx, *b.ParentProjectID)
			if err != nil {
				return nil, err
			}
			if parent.DeletedAt != nil {
				return nil, fmt.Errorf("%w: the parent %s was deleted; detach or delete the branch", ErrConflict, parent.Name)
			}
			if parent.OrgID != b.OrgID {
				return nil, fmt.Errorf("%w: the parent is in another organisation; detach or delete the branch", ErrConflict)
			}
			d, err := authz.Can(ctx, q, p.Actor, authz.BranchManage, authz.Resource{OrgID: parent.OrgID, ProjectID: parent.ID})
			if err != nil {
				return nil, err
			}
			if !d.Allowed {
				return nil, fmt.Errorf("%w: a reset copies %s again, which needs the developer role (or above) on it", ErrForbidden, parent.Name)
			}
			schemaOnly := b.BranchSchemaOnly != nil && *b.BranchSchemaOnly
			if !schemaOnly && parent.SensitiveData && d.ProjectRole != authz.ProjectAdmin {
				return nil, fmt.Errorf("%w: %s contains sensitive data; refilling a full-data branch needs the project admin role on it", ErrForbidden, parent.Name)
			}
			src := p.Source
			if src == "" && b.BranchSource != nil {
				src = *b.BranchSource
			}
			switch src {
			case "":
				src = SourceBackup
			case SourceBackup, SourceLive:
			default:
				return nil, fmt.Errorf("%w: source must be backup or live", provision.ErrInvalid)
			}
			if src == SourceBackup {
				if _, err := q.LatestSucceededBackup(ctx, b.ParentProjectID); errors.Is(err, pgx.ErrNoRows) {
					return nil, fmt.Errorf("%w: the parent has no backup yet; reset from live instead", provision.ErrConflict)
				} else if err != nil {
					return nil, err
				}
			}
			return map[string]any{"branch": fillParams{Parent: *b.ParentProjectID, Source: src, SchemaOnly: schemaOnly, CopyFiles: b.BranchCopyFiles}}, nil
		})
}

// runReset holds clients at the pooler, recreates the database with the
// same roles and verifiers, refills it, and resumes the route.
func (s *Service) runReset(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	params, err := opFill(op)
	if err != nil {
		return err
	}
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	if err := s.projects.Pooler().Kill(ctx, store.PoolerNames(p)...); err != nil {
		return fmt.Errorf("pooler KILL: %w", err)
	}
	if err := log.Info(ctx, "pooler", "clients disconnected; new connections wait"); err != nil {
		return err
	}
	// The files stored until now that the refilled rows don't name are the
	// branch's to tidy (the garbage list went with the old database).
	droppedAt := time.Now()
	if err := s.projects.RecreateDatabase(ctx, p, log); err != nil {
		return err
	}
	if err := s.fill(ctx, p, params, log); err != nil {
		return err
	}
	// Members' logins live outside the database; their grants inside it
	// come back here (V2 §3.5).
	if err := s.projects.SyncMemberRoles(ctx, p, log); err != nil {
		return err
	}
	// The branch's API, if it has one, keeps its ref and keys; the copy's
	// sessions go and its roles and schemas are checked again. A branch
	// made with copy_files gets the parent's files again, and the files no
	// row names any more are removed.
	if s.API != nil {
		if err := s.API.EnableBranch(ctx, p, log); err != nil {
			return fmt.Errorf("backend services: %w", err)
		}
		if err := s.API.BranchFiles(ctx, p, params.Parent, services.BranchFiles{Copy: params.CopyFiles, ResetAt: droppedAt}, log); err != nil {
			return fmt.Errorf("files: %w", err)
		}
	}
	if err := s.reopen(ctx, p, provision.StatusActive); err != nil {
		return err
	}
	return log.Info(ctx, "done", "branch %s reset from its parent; its URL and credentials are unchanged", p.Name)
}

// failReset resumes a branch whose reset gave up, marked error: its data
// may be partial, and another reset (or delete) fixes it.
func (s *Service) failReset(ctx context.Context, op store.Operation, log *jobs.StepLogger, cause error) error {
	p, err := s.projects.ProjectFor(ctx, op)
	if err != nil {
		return err
	}
	_ = log.Warn(ctx, "rollback", "the reset failed (%v); the branch may be partly restored. Reset it again or delete it", cause)
	return s.reopen(ctx, p, provision.StatusError)
}

func (s *Service) reopen(ctx context.Context, p store.Project, status string) error {
	if err := s.projects.Pooler().Resume(ctx, store.PoolerNames(p)...); err != nil && !strings.Contains(err.Error(), "is not paused") {
		return fmt.Errorf("pooler RESUME: %w", err)
	}
	return store.New(s.db).SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: status})
}

// DetachParams asks for a branch to become a standalone project.
type DetachParams struct {
	ID uuid.UUID
	// Actor is who asks. Detaching makes a new project of the organisation,
	// so they need org.create_project, as for any new project.
	Actor authz.Actor
	// CheckCreateProject, when set, applies the projects quota (tenancy),
	// which branches don't count towards.
	CheckCreateProject func(ctx context.Context, orgID uuid.UUID) error
}

// Detach turns a branch into an ordinary standalone project, under its
// row lock: it must be active with no operation pending (a reset in flight
// would refill it from its former parent).
func (s *Service) Detach(ctx context.Context, p DetachParams) (store.Project, error) {
	var out store.Project
	err := s.projects.WithIdleProject(ctx, p.ID, []string{provision.StatusActive}, func(tx pgx.Tx, b store.Project) error {
		if b.ParentProjectID == nil {
			return fmt.Errorf("%w: %w", provision.ErrInvalid, ErrNotBranch)
		}
		q := store.New(tx)
		d, err := authz.Can(ctx, q, p.Actor, authz.OrgCreateProject, authz.Resource{OrgID: b.OrgID})
		if err != nil {
			return err
		}
		if !d.Allowed {
			return fmt.Errorf("%w: detaching makes a standalone project, which needs permission to create projects in the organisation", ErrForbidden)
		}
		if p.CheckCreateProject != nil {
			if err := p.CheckCreateProject(ctx, b.OrgID); err != nil {
				return err
			}
		}
		out, err = q.DetachBranch(ctx, b.ID)
		return err
	})
	return out, err
}

// SetExpiry moves a branch's expiry (nil: keep it); the 24-hour warning
// is sent again before the new time.
func (s *Service) SetExpiry(ctx context.Context, id uuid.UUID, at *time.Time) (store.Project, error) {
	if at != nil {
		now := s.cfg.Now()
		if !at.After(now) || at.After(now.Add(MaxTTL)) {
			return store.Project{}, fmt.Errorf("%w: the expiry must be in the next 30 days", provision.ErrInvalid)
		}
	}
	p, err := store.New(s.db).UpdateBranch(ctx, store.UpdateBranchParams{ID: id, SetExpiry: true, ExpiresAt: at})
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("%w: %w", provision.ErrInvalid, ErrNotBranch)
	}
	return p, err
}

// SetBackups turns nightly backups on or off for a branch.
func (s *Service) SetBackups(ctx context.Context, id uuid.UUID, on bool) (store.Project, error) {
	p, err := store.New(s.db).UpdateBranch(ctx, store.UpdateBranchParams{ID: id, BranchBackups: &on})
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("%w: %w", provision.ErrInvalid, ErrNotBranch)
	}
	return p, err
}

// Sweep deletes expired branches (without a final backup unless the
// branch takes backups) and emails creators 24 hours before expiry. It is
// safe to run often and from several servers.
func (s *Service) Sweep(ctx context.Context) error {
	now := s.cfg.Now()
	q := store.New(s.db)
	var errs []error
	expired, err := q.ExpiredBranches(ctx, now)
	if err != nil {
		return err
	}
	for _, b := range expired {
		if _, err := s.expire(ctx, b.ID, now); err != nil && !errors.Is(err, provision.ErrConflict) {
			errs = append(errs, fmt.Errorf("expire branch %s: %w", b.ID, err))
		} else if err == nil {
			s.log.Info("branch expired; deleting", "project_id", b.ID, "expired_at", b.ExpiresAt)
		}
	}
	soon, err := q.BranchesExpiringSoon(ctx, store.BranchesExpiringSoonParams{Now: now, Before: now.Add(warnBefore)})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, b := range soon {
		err := s.mail.Send(ctx, mail.Compose([]string{b.Email}, fmt.Sprintf("Branch %s expires in less than a day", b.Name), mail.Content{
			Preheader: "Extend it to keep it a little longer, or detach it to keep it for good.",
			Title:     fmt.Sprintf("Branch %s expires soon", b.Name),
			Blocks: []mail.Block{
				mail.P("Hello,"),
				mail.P(fmt.Sprintf("Your branch %q of %s will be deleted on %s.", b.Name, b.ParentName, b.ExpiresAt.UTC().Format("2 January 2006 at 15:04 MST"))),
				mail.P("To keep it a little longer, extend it from the branches page, or with `xenibase branch extend`. To keep it for good, detach it from its parent."),
				mail.Button("Open the branches", fmt.Sprintf("%s/projects/%s/branches", s.cfg.PublicURL, b.ID)),
			},
			SignOff: mail.SignOff,
		}))
		if err != nil && !errors.Is(err, mail.ErrNotConfigured) {
			errs = append(errs, fmt.Errorf("expiry email for %s: %w", b.ID, err))
			continue
		}
		if err := q.MarkBranchExpiryNotified(ctx, b.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// expire deletes a branch the sweep listed as expired, if its locked row
// still is one: a branch extended or detached since the list was read is
// skipped (ErrConflict). Whether it takes a final backup is the row's too.
func (s *Service) expire(ctx context.Context, id uuid.UUID, now time.Time) (store.Operation, error) {
	return s.projects.DeleteChecked(ctx, id, nil, func(p store.Project) (bool, error) {
		if p.ParentProjectID == nil || p.ExpiresAt == nil || p.ExpiresAt.After(now) {
			return false, fmt.Errorf("%w: %s is no longer an expired branch", provision.ErrConflict, p.Name)
		}
		return !p.BranchBackups, nil
	})
}

// Run sweeps every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("branch sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// List returns a parent's live branches.
func (s *Service) List(ctx context.Context, parent store.Project) ([]store.Project, error) {
	return store.New(s.db).ListBranches(ctx, store.ListBranchesParams{ParentProjectID: &parent.ID, OrgID: parent.OrgID})
}
