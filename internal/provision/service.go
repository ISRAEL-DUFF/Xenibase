package provision

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/store"
)

// Operation kinds implemented here.
const (
	KindCreate = "create"
	KindDelete = "delete"
	KindRotate = "rotate"
)

// Project statuses.
const (
	StatusProvisioning = "provisioning"
	StatusActive       = "active"
	StatusDeleting     = "deleting"
	StatusRestoring    = "restoring"
	StatusPromoting    = "promoting"
	StatusDemoting     = "demoting"
	StatusMoving       = "moving"
	StatusUpgrading    = "upgrading"
	StatusDeleted      = "deleted"
	StatusError        = "error"
)

var (
	// ErrNotFound means the project does not exist (or was deleted).
	ErrNotFound = errors.New("project not found")
	// ErrConflict means the project is busy or in the wrong state.
	ErrConflict = errors.New("conflict")
	// ErrNoCapacity means no shared instance can take a new project.
	ErrNoCapacity = errors.New("no shared cluster available")
)

// Config holds the connection details handed to clients and those the
// control plane uses to smoke-test the poolers.
type Config struct {
	// Public connection info, as clients see it (spec §5.1). DBHostFunc,
	// when set, supplies the host at call time (it is operator-editable)
	// and DBHost is its fallback.
	DBHost      string
	DBHostFunc  func() string
	SessionPort int // session-mode pooler, 5432 in production
	PooledPort  int // transaction-mode pooler, 6543 in production
	SSLMode     string

	// SmokeSessionAddr and SmokePooledAddr are host:port of the two poolers
	// as xenibase-server reaches them; they default to the public ones.
	SmokeSessionAddr string
	SmokePooledAddr  string
	// SmokeSSLMode is the sslmode for smoke tests (default "prefer").
	SmokeSSLMode string
	// AdminSSLMode is the sslmode for admin connections to clusters
	// (default "prefer").
	AdminSSLMode string
	// PGVersions are the supported Postgres majors (V3 §2.4), newest last;
	// the newest is the default (default [17, 18]).
	PGVersions []int
	// HomeRegion is where projects go when they name no region (V3 §6.1);
	// RegionHost returns a region's pooler hostname ("" for the database
	// host above).
	HomeRegion string
	RegionHost func(region string) string
}

// DefaultPGVersions are the Postgres majors Xenibase supports by default.
var DefaultPGVersions = []int{17, 18}

func (c *Config) setDefaults() {
	if c.SSLMode == "" {
		c.SSLMode = "require"
	}
	if c.SmokeSessionAddr == "" {
		c.SmokeSessionAddr = net.JoinHostPort(c.DBHost, strconv.Itoa(c.SessionPort))
	}
	if c.SmokePooledAddr == "" {
		c.SmokePooledAddr = net.JoinHostPort(c.DBHost, strconv.Itoa(c.PooledPort))
	}
	if c.SmokeSSLMode == "" {
		c.SmokeSSLMode = "prefer"
	}
	if c.AdminSSLMode == "" {
		c.AdminSSLMode = "prefer"
	}
	if len(c.PGVersions) == 0 {
		c.PGVersions = DefaultPGVersions
	}
	slices.Sort(c.PGVersions)
}

// PGVersions are the supported Postgres majors, oldest first.
func (s *Service) PGVersions() []int { return slices.Clone(s.cfg.PGVersions) }

func joinInts(xs []int) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strconv.Itoa(x)
	}
	return strings.Join(out, ", ")
}

// Service runs project provisioning.
type Service struct {
	db      *pgxpool.Pool
	keyring *crypto.Keyring
	pooler  *pooler.Manager
	cfg     Config
	log     *slog.Logger
	// versionsSeeded: the configured majors are in pg_versions.
	versionsSeeded atomic.Bool

	// FinalBackup, if set, takes the final backup before a delete (spec
	// §6.2). The backup service sets it.
	FinalBackup func(ctx context.Context, p store.Project, log *jobs.StepLogger) error
	// Instances runs dedicated instances; nil disables the dedicated tier.
	Instances InstanceManager
	// RefreshWebhooks, when set, rebuilds a project's webhook schema after
	// its database was replaced or copied (the webhooks service). Without
	// it, whatever came with the data is dropped.
	RefreshWebhooks func(ctx context.Context, p store.Project) error
	// LoginGate, when set, reports whether a project's logins may connect:
	// false while its storage is hard-locked or its organisation is
	// suspended (V2 §10.4, §10.8). The tenancy service sets it.
	LoginGate func(ctx context.Context, p store.Project) (bool, error)
	// DatabaseRecreated, when set, hears that p's database was dropped and
	// made again empty (a restore in place, a branch reset, a move's
	// target): backend services re-apply their schemas once it is filled.
	DatabaseRecreated func(ctx context.Context, p store.Project)
}

// ErrNoDedicated means the dedicated tier is not available.
// ErrUnreachable is a project's database instance that did not answer: down,
// stopped, or its node offline. Retrying later may work.
var ErrUnreachable = errors.New("the project's database is not reachable")

var ErrNoDedicated = errors.New("the dedicated tier is not available on this server")

// NewService returns a Service.
func NewService(db *pgxpool.Pool, keyring *crypto.Keyring, pm *pooler.Manager, cfg Config, log *slog.Logger) *Service {
	cfg.setDefaults()
	return &Service{db: db, keyring: keyring, pooler: pm, cfg: cfg, log: log}
}

// Kinds returns the operation kinds this service handles.
func (s *Service) Kinds() map[string]jobs.Kind {
	kinds := map[string]jobs.Kind{
		// A dedicated create may wait up to 25 minutes for a host (V4.1 §5.3).
		KindCreate: {Handler: s.runCreate, OnFail: s.rollbackCreate, MaxAttempts: 3, Timeout: 45 * time.Minute},
		KindRotate: {Handler: s.runRotate, OnFail: s.rollbackRotate, MaxAttempts: 3},
		KindDelete: {Handler: s.runDelete, OnFail: s.failDelete, MaxAttempts: 5},

		KindApplySettings: {Handler: s.runApplySettings, MaxAttempts: 5},
		KindDropDBUser:    {Handler: s.runDropDBUser, MaxAttempts: 10},
	}
	for k, v := range s.opaqueKinds() {
		kinds[k] = v
	}
	return kinds
}

// Connection is how clients reach a project. URLs carry no password.
type Connection struct {
	Host        string
	SessionPort int
	PooledPort  int
	Database    string
	User        string
	SSLMode     string
}

// ConnectionFor returns the connection info for a project.
func (s *Service) ConnectionFor(p store.Project) Connection {
	host := s.cfg.DBHost
	if s.cfg.DBHostFunc != nil {
		if h := s.cfg.DBHostFunc(); h != "" {
			host = h
		}
	}
	if s.cfg.RegionHost != nil {
		if h := s.cfg.RegionHost(p.Region); h != "" {
			host = h
		}
	}
	return Connection{
		Host: host, SessionPort: s.cfg.SessionPort, PooledPort: s.cfg.PooledPort,
		Database: store.ClientDBName(p), User: p.OwnerRole, SSLMode: s.cfg.SSLMode,
	}
}

// PooledURL is the transaction-mode URL (app traffic, serverless).
func (c Connection) PooledURL(password string) string { return c.url(c.PooledPort, password) }

// SessionURL is the session-mode URL (migrations, session features).
func (c Connection) SessionURL(password string) string { return c.url(c.SessionPort, password) }

func (c Connection) url(port int, password string) string {
	userinfo := c.User
	if password != "" {
		userinfo += ":" + password // URL-safe by construction
	}
	return fmt.Sprintf("postgresql://%s@%s/%s?sslmode=%s",
		userinfo, net.JoinHostPort(c.Host, strconv.Itoa(port)), c.Database, c.SSLMode)
}

// CreateParams describes a new project.
type CreateParams struct {
	// OrgID owns the project (V2 §2.1).
	OrgID uuid.UUID
	// CreatorRole, when set, makes CreatedBy a project member with this
	// role (an org member creating a project becomes its admin, V2 §2.2).
	CreatorRole string
	Name        string
	Description *string
	CreatedBy   *uuid.UUID
	// Kind is the operation that provisions it: KindCreate by default, or
	// another kind (restore, import) whose handler builds on Prepare and
	// Publish. Params are merged into that operation's params.
	Kind   string
	Params map[string]any
	// Tier is TierShared (default) or TierDedicated. A dedicated project
	// gets its own instance on NodeID (or the least loaded dedicated node)
	// with Profile's limits and VolumeGB of disk.
	Tier     string
	NodeID   *uuid.UUID
	Profile  string
	VolumeGB int
	// InstanceID, for a dedicated project, is its instance's id (default:
	// new): a detached read replica's instance keeps its member's id,
	// which names its container and volume (V4 §7).
	InstanceID *uuid.UUID
	// MinMaxConnections floors a dedicated instance's max_connections: a
	// restore's or a detached replica's source value (Postgres won't replay
	// WAL from a server that allowed more connections).
	MinMaxConnections int
	// PgVersion is the Postgres major (0: the default, the newest
	// supported). Preview allows a major in preview (V4.1 §6.1).
	PgVersion int
	Preview   bool
	// Region is where the project runs (empty: the home region, or NodeID's
	// region); DataResidency keeps its data in that region's country, which
	// the region must offer (V3 §6.3). A branch takes its parent's.
	Region        string
	DataResidency bool
	// Branch makes the project a branch of another (V2 §8); it is always on
	// the shared tier.
	Branch *BranchSpec
	// Sensitive marks the project "contains sensitive data" (V2 §8.5).
	Sensitive bool
}

// BranchSpec describes a new branch.
type BranchSpec struct {
	ParentID   uuid.UUID
	Source     string // "backup" or "live"
	SchemaOnly bool
	// ExpiresAt is when the hourly sweep deletes it; nil keeps it.
	ExpiresAt *time.Time
}

// Tiers (spec §4).
const (
	TierShared    = "shared"
	TierDedicated = "dedicated"
)

// InstanceManager runs dedicated instances (the dedicated package). It is
// optional: without it, only the shared tier is available.
type InstanceManager interface {
	// Validate checks a dedicated create request and fills defaults.
	Validate(ctx context.Context, p *CreateParams) (Profile, error)
	// Ensure creates and starts the project's instance (idempotent),
	// restoring it first when the operation asks for a point-in-time
	// recovery, and waits until it serves.
	Ensure(ctx context.Context, op store.Operation, p store.Project, log *jobs.StepLogger) error
	// Provisioned runs once the project is active (the first base backup).
	Provisioned(ctx context.Context, p store.Project, log *jobs.StepLogger) error
	// Destroy removes the instance's container, volume, and WAL archive.
	Destroy(ctx context.Context, p store.Project, log *jobs.StepLogger) error
}

// Profile is a dedicated instance's size (spec §6.6 step 1).
type Profile struct {
	Name     string  `json:"name"`
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
}

// Created is returned once by Create. Password is never stored; this is the
// only time it exists outside the client.
type Created struct {
	Project   store.Project
	Operation store.Operation
	Password  string
}

// opSecrets is the encrypted handoff in operations.params.secrets, scrubbed
// when the operation finishes. The create and rotate flows need the
// plaintext password only to smoke-test the pooler.
type opSecrets struct {
	Password string `json:"password"` // base64 of the sealed password
}

type createParams struct {
	Secrets opSecrets `json:"secrets"`
}

type rotateParams struct {
	Verifier         string    `json:"verifier"`
	PreviousVerifier string    `json:"previous_verifier"`
	Secrets          opSecrets `json:"secrets"`
}

func passwordAAD(projectID uuid.UUID, purpose string) []byte {
	return []byte("operations.secrets.password:" + purpose + ":" + projectID.String())
}

func (s *Service) sealPassword(projectID uuid.UUID, purpose, password string) (opSecrets, error) {
	ct, err := s.keyring.Encrypt([]byte(password), passwordAAD(projectID, purpose))
	if err != nil {
		return opSecrets{}, err
	}
	return opSecrets{Password: base64.StdEncoding.EncodeToString(ct)}, nil
}

func (s *Service) openPassword(projectID uuid.UUID, purpose string, sec opSecrets) (string, error) {
	ct, err := base64.StdEncoding.DecodeString(sec.Password)
	if err != nil || sec.Password == "" {
		return "", errors.New("operation has no password handoff (already scrubbed?)")
	}
	pt, err := s.keyring.Decrypt(ct, passwordAAD(projectID, purpose))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// Create validates the request, records the project, and queues the create
// operation (spec §6.1).
func (s *Service) Create(ctx context.Context, p CreateParams) (Created, error) {
	if p.OrgID == uuid.Nil {
		return Created{}, fmt.Errorf("%w: a project needs an organisation", ErrInvalid)
	}
	name, err := ValidateName(p.Name)
	if err != nil {
		return Created{}, err
	}
	slug, err := Slugify(name)
	if err != nil {
		return Created{}, err
	}
	if p.Description != nil && len(*p.Description) > 1000 {
		return Created{}, fmt.Errorf("%w: description must be at most 1000 bytes", ErrInvalid)
	}
	password, err := GeneratePassword()
	if err != nil {
		return Created{}, err
	}
	verifier, err := crypto.SCRAMVerifier(password)
	if err != nil {
		return Created{}, err
	}
	tier := p.Tier
	if tier == "" {
		tier = TierShared
	}
	use := ForProject
	if p.Preview {
		use = ForPreviewProject
	}
	if p.PgVersion, err = s.CheckVersion(ctx, p.PgVersion, use); err != nil {
		return Created{}, err
	}
	if err := s.resolveRegion(ctx, &p); err != nil {
		return Created{}, err
	}
	var profile Profile
	var inst store.Instance
	defaults := store.DefaultSharedSettings()
	switch tier {
	case TierShared:
		inst, err = store.New(s.db).PickSharedInstance(ctx, store.PickSharedInstanceParams{OrgID: &p.OrgID, PgVersion: int32(p.PgVersion), Region: p.Region})
		if errors.Is(err, pgx.ErrNoRows) {
			return Created{}, fmt.Errorf("%w (no shared cluster in %s runs Postgres %d)", ErrNoCapacity, p.Region, p.PgVersion)
		}
		if err != nil {
			return Created{}, err
		}
	case TierDedicated:
		if s.Instances == nil {
			return Created{}, ErrNoDedicated
		}
		if profile, err = s.Instances.Validate(ctx, &p); err != nil {
			return Created{}, err
		}
		defaults = store.DefaultDedicatedSettings(p.VolumeGB)
	default:
		return Created{}, fmt.Errorf("%w: tier must be shared or dedicated", ErrInvalid)
	}
	settings, err := json.Marshal(defaults)
	if err != nil {
		return Created{}, err
	}

	kind := p.Kind
	if kind == "" {
		kind = KindCreate
	}
	// A random name rarely collides; retry with a fresh one if it does.
	for attempt := 0; ; attempt++ {
		dbName, role, err := Names()
		if err != nil {
			return Created{}, err
		}
		id := uuid.New()
		sec, err := s.sealPassword(id, kind, password)
		if err != nil {
			return Created{}, err
		}
		params := map[string]any{}
		for k, v := range p.Params {
			params[k] = v
		}
		params["secrets"] = sec

		var out Created
		err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			q := store.New(tx)
			instanceID := inst.ID
			if tier == TierDedicated {
				iid := uuid.New()
				if p.InstanceID != nil {
					iid = *p.InstanceID
				}
				prefix := "instances/" + iid.String() + "/wal-g"
				mem := int32(profile.MemoryMB)
				vol := int32(p.VolumeGB)
				maxConns := int32(max(MaxConnectionsFor(profile.MemoryMB), p.MinMaxConnections))
				ni, err := q.InsertInstance(ctx, store.InsertInstanceParams{
					ID: iid, NodeID: *p.NodeID, Kind: TierDedicated, PgVersion: int32(p.PgVersion), CpuLimit: numeric(profile.CPUs),
					MemLimitMb: &mem, VolumeGb: &vol, Profile: &profile.Name, WalgPrefix: &prefix, MaxConnections: &maxConns,
				})
				if err != nil {
					return err
				}
				instanceID = ni.ID
			}
			proj, err := q.InsertProject(ctx, store.InsertProjectParams{
				ID: id, OrgID: p.OrgID, Name: name, Slug: slug, DbName: dbName, OwnerRole: role,
				ScramVerifier: verifier, Tier: tier, InstanceID: instanceID, Settings: settings,
				Description: p.Description, CreatedBy: p.CreatedBy, Region: p.Region, DataResidency: p.DataResidency,
			})
			if err != nil {
				return err
			}
			if p.Branch != nil {
				if err := q.SetProjectBranch(ctx, store.SetProjectBranchParams{
					ID: proj.ID, ParentProjectID: &p.Branch.ParentID, BranchSource: &p.Branch.Source,
					BranchSchemaOnly: &p.Branch.SchemaOnly, ExpiresAt: p.Branch.ExpiresAt, SensitiveData: p.Sensitive,
				}); err != nil {
					return err
				}
				if err := q.CopyProjectMembers(ctx, store.CopyProjectMembersParams{BranchID: proj.ID, ParentID: p.Branch.ParentID, OrgID: p.OrgID}); err != nil {
					return err
				}
				if proj, err = q.GetProject(ctx, proj.ID); err != nil {
					return err
				}
			} else if p.Sensitive {
				if err := q.SetProjectSensitive(ctx, store.SetProjectSensitiveParams{ID: proj.ID, SensitiveData: true}); err != nil {
					return err
				}
				proj.SensitiveData = true
			}
			if p.CreatorRole != "" && p.CreatedBy != nil {
				if err := q.UpsertProjectMember(ctx, store.UpsertProjectMemberParams{
					ProjectID: proj.ID, UserID: *p.CreatedBy, OrgID: p.OrgID, Role: p.CreatorRole, AddedBy: p.CreatedBy,
				}); err != nil {
					return err
				}
			}
			op, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{
				Kind: kind, ProjectID: &proj.ID, CreatedBy: p.CreatedBy, Params: params,
			})
			if err != nil {
				return err
			}
			out = Created{Project: proj, Operation: op, Password: password}
			return nil
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && attempt < 5 {
			continue
		}
		if err != nil {
			return Created{}, err
		}
		return out, nil
	}
}

// Rotated is returned once by Rotate.
type Rotated struct {
	Project   store.Project
	Operation store.Operation
	Password  string
}

// Rotate queues a password rotation (spec §5.3). The new password works once
// the operation succeeds; the old one stops working at the same moment.
func (s *Service) Rotate(ctx context.Context, projectID uuid.UUID, by *uuid.UUID) (Rotated, error) {
	password, err := GeneratePassword()
	if err != nil {
		return Rotated{}, err
	}
	verifier, err := crypto.SCRAMVerifier(password)
	if err != nil {
		return Rotated{}, err
	}
	sec, err := s.sealPassword(projectID, KindRotate, password)
	if err != nil {
		return Rotated{}, err
	}
	var out Rotated
	err = s.withIdleProject(ctx, projectID, []string{StatusActive}, func(tx pgx.Tx, p store.Project) error {
		op, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{
			Kind: KindRotate, ProjectID: &p.ID, CreatedBy: by,
			Params: rotateParams{Verifier: verifier, PreviousVerifier: p.ScramVerifier, Secrets: sec},
		})
		out = Rotated{Project: p, Operation: op, Password: password}
		return err
	})
	return out, err
}

// Delete queues deletion (spec §6.2). confirmName must equal the project's
// name, as typed by the operator. skipFinalBackup skips the final backup
// (spec §6.2 step 2: on by default, can be skipped).
func (s *Service) Delete(ctx context.Context, projectID uuid.UUID, confirmName string, skipFinalBackup bool, by *uuid.UUID) (store.Operation, error) {
	var out store.Operation
	err := s.withIdleProject(ctx, projectID, []string{StatusActive, StatusError}, func(tx pgx.Tx, p store.Project) error {
		if confirmName != p.Name {
			return fmt.Errorf("%w: confirm must match the project name exactly", ErrInvalid)
		}
		// A parent goes only once its branches are deleted or detached (V2 §8.4).
		if n, err := store.New(tx).CountLiveBranches(ctx, &p.ID); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("%w: the project has %d branch(es); delete or detach them first", ErrConflict, n)
		}
		if err := store.New(tx).SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: StatusDeleting}); err != nil {
			return err
		}
		// Branches are disposable: no final backup unless they take backups.
		if p.ParentProjectID != nil && !p.BranchBackups {
			skipFinalBackup = true
		}
		op, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: KindDelete, ProjectID: &p.ID, CreatedBy: by,
			Params: map[string]any{"skip_final_backup": skipFinalBackup}})
		out = op
		return err
	})
	return out, err
}

// EnqueueExclusiveTx is EnqueueExclusive with params built by f inside the
// transaction (which can also insert rows and refuse).
func (s *Service) EnqueueExclusiveTx(ctx context.Context, projectID uuid.UUID, allowed []string, status, kind string, by *uuid.UUID,
	f func(pgx.Tx, store.Project) (any, error)) (store.Operation, error) {
	var out store.Operation
	err := s.withIdleProject(ctx, projectID, allowed, func(tx pgx.Tx, p store.Project) error {
		params, err := f(tx, p)
		if err != nil {
			return err
		}
		if status != "" {
			if err := store.New(tx).SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: status}); err != nil {
				return err
			}
		}
		out, err = jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: kind, ProjectID: &p.ID, CreatedBy: by, Params: params})
		return err
	})
	return out, err
}

// EnqueueExclusive queues an operation of kind on a project that is in one
// of the allowed statuses and has no other operation pending, optionally
// moving it to status (empty: unchanged). check, if set, runs on the locked
// project first and can refuse.
func (s *Service) EnqueueExclusive(ctx context.Context, projectID uuid.UUID, allowed []string, status, kind string, params any, by *uuid.UUID, check func(store.Project) error) (store.Operation, error) {
	var out store.Operation
	err := s.withIdleProject(ctx, projectID, allowed, func(tx pgx.Tx, p store.Project) error {
		if check != nil {
			if err := check(p); err != nil {
				return err
			}
		}
		if status != "" {
			if err := store.New(tx).SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: status}); err != nil {
				return err
			}
		}
		op, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: kind, ProjectID: &p.ID, CreatedBy: by, Params: params})
		out = op
		return err
	})
	return out, err
}

// withIdleProject locks a live project, checks its status and that no other
// operation is pending on it, and runs f in the same transaction. One
// operation per project at a time keeps the flows from interleaving.
func (s *Service) withIdleProject(ctx context.Context, id uuid.UUID, allowed []string, f func(pgx.Tx, store.Project) error) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		p, err := q.GetLiveProjectForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		ok := false
		for _, st := range allowed {
			ok = ok || p.Status == st
		}
		if !ok {
			return fmt.Errorf("%w: project is %s", ErrConflict, p.Status)
		}
		busy, err := q.ProjectHasActiveOperation(ctx, &id)
		if err != nil {
			return err
		}
		if busy {
			return fmt.Errorf("%w: another operation is in progress on this project", ErrConflict)
		}
		return f(tx, p)
	})
}

// Get returns a live project.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (store.Project, error) {
	p, err := store.New(s.db).GetProject(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && p.DeletedAt != nil) {
		return store.Project{}, ErrNotFound
	}
	return p, err
}

// List returns live projects, newest first.
func (s *Service) List(ctx context.Context, status *string, limit int) ([]store.Project, error) {
	return store.New(s.db).ListLiveProjects(ctx, store.ListLiveProjectsParams{Status: status, MaxRows: int32(limit)})
}

// resolveRegion settles where a new project runs: a branch where its
// parent is, a chosen node where that node is, else the region asked for
// or the home region. Data residency needs a region that offers it.
func (s *Service) resolveRegion(ctx context.Context, p *CreateParams) error {
	q := store.New(s.db)
	switch {
	case p.Branch != nil:
		parent, err := q.GetProject(ctx, p.Branch.ParentID)
		if err != nil {
			return err
		}
		p.Region, p.DataResidency = parent.Region, parent.DataResidency
	case p.NodeID != nil:
		n, err := q.GetNode(ctx, *p.NodeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no such node", ErrInvalid)
		} else if err != nil {
			return err
		}
		if p.Region != "" && p.Region != n.Region {
			return fmt.Errorf("%w: node %s is in %s, not %s", ErrInvalid, n.Name, n.Region, p.Region)
		}
		p.Region = n.Region
	case p.Region == "":
		p.Region = s.cfg.HomeRegion
		if p.Region == "" {
			p.Region = "eu-central"
		}
	}
	r, err := q.GetRegion(ctx, p.Region)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no region %q", ErrInvalid, p.Region)
	}
	if err != nil {
		return err
	}
	// A hidden region takes new projects only from its early-access
	// organisations (a rehearsal, a first customer); a branch follows its
	// parent wherever it is.
	if r.Status != "active" && p.Branch == nil {
		ok, err := q.RegionHasEarlyAccess(ctx, store.RegionHasEarlyAccessParams{RegionID: r.ID, OrgID: p.OrgID})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: no region %q", ErrInvalid, p.Region)
		}
	}
	if p.DataResidency && !r.Residency {
		return fmt.Errorf("%w: %s doesn't offer data residency", ErrInvalid, r.Name)
	}
	return nil
}
