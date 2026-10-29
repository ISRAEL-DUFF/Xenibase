// Package services runs backend services' control-plane side (V4 §2):
// enabling and disabling them per project (roles and xb_* schemas in the
// project database, API keys, the signing key), keeping the roles current
// when a project moves, and the configuration feed and reports xenibase-edge
// uses.
package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/backup"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/jwtes"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
)

// Operation kinds (V4 §11.1).
const (
	KindEnable  = "enable_services"
	KindDisable = "disable_services"
)

// Errors.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInvalid  = errors.New("invalid")
)

// edgeConnLimit caps the edge login's sessions per project database (auth),
// requestConnLimit each request role's, hookConnLimit the hook role's.
const (
	edgeConnLimit    = 20
	requestConnLimit = 20
	hookConnLimit    = 10
)

// Config is the service's configuration.
type Config struct {
	// Domain is the API hostname suffix: a project's URL is
	// https://<ref>.<Domain> (V4 §2.1). RegionDomain, when set, gives a
	// region its own.
	Domain       string
	RegionDomain func(region string) string
	// EdgeSecret signs xenibase-edge's requests. Empty disables the feed.
	// It serves the regions without their own API domain; an edge for
	// another region needs its own credential (EdgeScope).
	EdgeSecret string
	// EdgeSecretAllRegions lets EdgeSecret serve every region, as before
	// edges had their own credentials (XENIBASE_EDGE_SECRET_ALL_REGIONS).
	EdgeSecretAllRegions bool
	// CaptchaVerifyURL is Turnstile's siteverify endpoint (tests point it
	// at a fake); empty is Cloudflare's.
	CaptchaVerifyURL string
}

// Service is the control plane's side of backend services.
type Service struct {
	db       *pgxpool.Pool
	keyring  *crypto.Keyring
	projects *provision.Service
	cfg      Config
	log      *slog.Logger
	// Waker resumes a paused project (the free tier, V3 §4.2).
	Waker func(ctx context.Context, projectID uuid.UUID) error
	// Mail is the platform's email, for projects without their own SMTP.
	Mail *mail.Service
	// Phone is the platform's SMS and WhatsApp, for projects without their
	// own provider (V4 §6.2).
	Phone PlatformPhone
	// Outbound makes hook calls (V4 §6.5).
	Outbound *outbound.Service
	// Files resolves the object store for files of projects in a region
	// (V4 §5.1); nil turns storage off.
	Files func(ctx context.Context, region string, residency bool) (storage.Target, error)
	// CDN purges public files when their bucket goes private (V4 §5.4).
	CDN CDNPurger
	// StorageGrace is how long replaced and deleted files' bytes stay for
	// downloads in flight (a minute by default; tests shorten it).
	StorageGrace time.Duration
	emailKick    chan struct{}
	// secrets holds migrations' source credentials while they run.
	secrets *backup.Ephemeral
}

// New returns the service.
func New(db *pgxpool.Pool, projects *provision.Service, cfg Config, log *slog.Logger) *Service {
	return &Service{db: db, keyring: projects.Keyring(), projects: projects, cfg: cfg, log: log, emailKick: make(chan struct{}, 1), secrets: backup.NewEphemeral()}
}

// Kinds are the operations it runs.
func (s *Service) Kinds() map[string]jobs.Kind {
	kinds := map[string]jobs.Kind{
		KindEnable:  {Handler: s.runEnable, MaxAttempts: 3, Timeout: 10 * time.Minute},
		KindDisable: {Handler: s.runDisable, MaxAttempts: 3, Timeout: 10 * time.Minute},
		// A branch's files (V4.1 §9.5): retried, skipping what is copied.
		KindCopyBranchFiles: {Handler: s.runCopyBranchFiles, MaxAttempts: 3, Timeout: 6 * time.Hour},
	}
	for k, v := range s.supabaseKinds() {
		kinds[k] = v
	}
	return kinds
}

// URL is a project's API base URL.
func (s *Service) URL(ref, region string) string {
	d := s.cfg.Domain
	if s.cfg.RegionDomain != nil {
		if r := s.cfg.RegionDomain(region); r != "" {
			d = r
		}
	}
	if d == "" {
		return ""
	}
	return "https://" + ref + "." + d
}

// FeedEnabled reports whether xenibase-edge can connect.
func (s *Service) FeedEnabled() bool { return s.cfg.EdgeSecret != "" }

// CreatedKey is an API key made now; Key is shown once for a secret key.
type CreatedKey struct {
	store.ProjectApiKey
	Key string
}

// Enabled is what Enable returns: the operation, and the first keys when it
// made them.
type Enabled struct {
	Services  store.ProjectService
	Operation store.Operation
	Keys      []CreatedKey
}

// Enable queues turning backend services on for p. The first time (and
// after a disable, which revokes keys) it makes a publishable and a secret
// key, returned here once.
func (s *Service) Enable(ctx context.Context, projectID uuid.UUID, by *uuid.UUID) (Enabled, error) {
	var out Enabled
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		p, err := q.GetLiveProjectForUpdate(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if p.Status != provision.StatusActive {
			return fmt.Errorf("%w: the project is %s", ErrConflict, p.Status)
		}
		if busy, err := q.ProjectHasActiveOperation(ctx, &p.ID); err != nil {
			return err
		} else if busy {
			return fmt.Errorf("%w: another operation is in progress on this project", ErrConflict)
		}
		svc, err := ensureRow(ctx, q, p.ID)
		if err != nil {
			return err
		}
		if svc.Enabled {
			return fmt.Errorf("%w: backend services are already enabled", ErrConflict)
		}
		keys, err := q.ListAPIKeys(ctx, p.ID)
		if err != nil {
			return err
		}
		live := 0
		for _, k := range keys {
			if k.RevokedAt == nil {
				live++
			}
		}
		if live == 0 {
			for _, kind := range []string{KindPublishable, KindSecret} {
				k, err := insertKey(ctx, q, p.ID, kind, "default", by)
				if err != nil {
					return err
				}
				out.Keys = append(out.Keys, k)
			}
		}
		out.Services = svc
		out.Operation, err = jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: KindEnable, ProjectID: &p.ID, CreatedBy: by})
		return err
	})
	return out, err
}

// Disable queues turning backend services off: the edge stops serving p,
// its keys are revoked and the edge login is switched off. The xb_*
// schemas and their data stay (V4 §2.4).
func (s *Service) Disable(ctx context.Context, projectID uuid.UUID, by *uuid.UUID) (store.Operation, error) {
	var op store.Operation
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		p, err := q.GetLiveProjectForUpdate(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		svc, err := q.GetProjectServices(ctx, p.ID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !svc.Enabled) {
			return fmt.Errorf("%w: backend services aren't enabled", ErrConflict)
		}
		if err != nil {
			return err
		}
		if busy, err := q.ProjectHasActiveOperation(ctx, &p.ID); err != nil {
			return err
		} else if busy {
			return fmt.Errorf("%w: another operation is in progress on this project", ErrConflict)
		}
		// The edge stops at once; the rest follows in the operation.
		if _, err := q.SetServicesEnabled(ctx, store.SetServicesEnabledParams{ProjectID: p.ID, Enabled: false}); err != nil {
			return err
		}
		if err := q.RevokeProjectAPIKeys(ctx, p.ID); err != nil {
			return err
		}
		op, err = jobs.Enqueue(ctx, tx, jobs.EnqueueParams{Kind: KindDisable, ProjectID: &p.ID, CreatedBy: by})
		return err
	})
	return op, err
}

func ensureRow(ctx context.Context, q *store.Queries, projectID uuid.UUID) (store.ProjectService, error) {
	svc, err := q.GetProjectServices(ctx, projectID)
	if err == nil {
		return svc, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return svc, err
	}
	for range 5 {
		ref, err := NewRef()
		if err != nil {
			return svc, err
		}
		svc, err = q.CreateProjectServices(ctx, store.CreateProjectServicesParams{ProjectID: projectID, Ref: ref})
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			continue // ref taken: another
		}
		return svc, err
	}
	return svc, errors.New("could not pick a free project reference")
}

func insertKey(ctx context.Context, q *store.Queries, projectID uuid.UUID, kind, name string, by *uuid.UUID) (CreatedKey, error) {
	key, hash, prefix, err := GenerateKey(kind)
	if err != nil {
		return CreatedKey{}, err
	}
	var display *string
	if kind == KindPublishable {
		display = &key
	}
	k, err := q.InsertAPIKey(ctx, store.InsertAPIKeyParams{ProjectID: projectID, Kind: kind, Name: name, KeyHash: hash,
		Prefix: prefix, Display: display, CreatedBy: by})
	return CreatedKey{ProjectApiKey: k, Key: key}, err
}

// CreateKey makes another key (for rotation without downtime).
func (s *Service) CreateKey(ctx context.Context, projectID uuid.UUID, kind, name string, by *uuid.UUID) (CreatedKey, error) {
	name = strings.TrimSpace(name)
	if kind != KindPublishable && kind != KindSecret {
		return CreatedKey{}, fmt.Errorf("%w: kind must be publishable or secret", ErrInvalid)
	}
	if name == "" || len(name) > 64 {
		return CreatedKey{}, fmt.Errorf("%w: a key needs a name of 1 to 64 characters", ErrInvalid)
	}
	q := store.New(s.db)
	svc, err := q.GetProjectServices(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !svc.Enabled) {
		return CreatedKey{}, fmt.Errorf("%w: backend services aren't enabled", ErrConflict)
	}
	if err != nil {
		return CreatedKey{}, err
	}
	return insertKey(ctx, q, projectID, kind, name, by)
}

// RevokeKey revokes a key; the edge refuses it within seconds.
func (s *Service) RevokeKey(ctx context.Context, projectID, keyID uuid.UUID) (store.ProjectApiKey, error) {
	k, err := store.New(s.db).RevokeAPIKey(ctx, store.RevokeAPIKeyParams{ID: keyID, ProjectID: projectID})
	if errors.Is(err, pgx.ErrNoRows) {
		return k, fmt.Errorf("%w: no such live key", ErrNotFound)
	}
	return k, err
}

// Settings are a project's gateway settings as stored (zero is the
// default).
type Settings struct {
	StatementTimeoutMs   int  `json:"statement_timeout_ms,omitempty"`
	RatePerIP            int  `json:"rate_per_ip,omitempty"`
	RatePerKey           int  `json:"rate_per_key,omitempty"`
	AllowSecretInBrowser bool `json:"allow_secret_in_browser,omitempty"`
	MaxQueryCost         int  `json:"max_query_cost,omitempty"`
	// ReplicaReads sends publishable-key data API GETs to the read
	// replicas by default (V4 §7).
	ReplicaReads bool `json:"replica_reads,omitempty"`
	// CacheTTLSeconds caches anonymous reads of the listed tables
	// ("schema.table") and functions ("rpc.name", "rpc.schema.name") on the
	// edge for up to that many seconds (V4.1 §10).
	CacheTTLSeconds map[string]int `json:"cache_ttl_seconds,omitempty"`
}

// MaxCacheTTLSeconds is the longest an anonymous read may be cached, and
// MaxCachedRelations how many relations may be listed.
const (
	MaxCacheTTLSeconds = 3600
	MaxCachedRelations = 100
)

var cacheNameRe = regexp.MustCompile(`^(rpc\.([A-Za-z_][A-Za-z0-9_$]*\.)?|[A-Za-z_][A-Za-z0-9_$]*\.)[A-Za-z_][A-Za-z0-9_$]*$`)

// Defaults.
const (
	DefaultStatementTimeoutMs = 8000
	DefaultRatePerIP          = 600   // per minute
	DefaultRatePerKey         = 12000 // per minute
	MaxStatementTimeoutMs     = 15000
	// DefaultMaxQueryCost is the planner cost above which a data API read
	// is refused (a sequential scan of a few million rows).
	DefaultMaxQueryCost = 1_000_000
)

// DecodeSettings reads stored settings.
func DecodeSettings(raw []byte) (Settings, error) {
	var st Settings
	if len(raw) == 0 {
		return st, nil
	}
	return st, json.Unmarshal(raw, &st)
}

// Validate checks the settings' ranges.
func (st Settings) Validate() error {
	switch {
	case st.StatementTimeoutMs < 0 || st.StatementTimeoutMs > MaxStatementTimeoutMs:
		return fmt.Errorf("%w: statement_timeout_ms must be up to %d", ErrInvalid, MaxStatementTimeoutMs)
	case st.RatePerIP < 0 || st.RatePerIP > 1_000_000, st.RatePerKey < 0 || st.RatePerKey > 10_000_000:
		return fmt.Errorf("%w: rate limits are requests per minute", ErrInvalid)
	case st.MaxQueryCost < 0:
		return fmt.Errorf("%w: max_query_cost can't be negative", ErrInvalid)
	case len(st.CacheTTLSeconds) > MaxCachedRelations:
		return fmt.Errorf("%w: cache at most %d tables and functions", ErrInvalid, MaxCachedRelations)
	}
	for name, ttl := range st.CacheTTLSeconds {
		if !cacheNameRe.MatchString(name) {
			return fmt.Errorf("%w: cache_ttl_seconds names a table as schema.table and a function as rpc.name or rpc.schema.name, not %q", ErrInvalid, name)
		}
		if ttl < 1 || ttl > MaxCacheTTLSeconds {
			return fmt.Errorf("%w: %s's cache TTL must be 1 to %d seconds", ErrInvalid, name, MaxCacheTTLSeconds)
		}
	}
	return nil
}

var schemaNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// validExposure checks exposed schemas and public tables' names.
func validExposure(schemas, public []string) error {
	if len(schemas) == 0 || len(schemas) > 10 {
		return fmt.Errorf("%w: expose 1 to 10 schemas", ErrInvalid)
	}
	for _, sc := range schemas {
		if !schemaNameRe.MatchString(sc) || strings.HasPrefix(sc, "pg_") || strings.HasPrefix(sc, "xb_") ||
			sc == "information_schema" || sc == "xenibase" {
			return fmt.Errorf("%w: %q can't be exposed", ErrInvalid, sc)
		}
	}
	if len(public) > 200 {
		return fmt.Errorf("%w: at most 200 public tables", ErrInvalid)
	}
	for _, t := range public {
		if t == "" || len(t) > 130 || strings.ContainsAny(t, " ,\\\"'") {
			return fmt.Errorf("%w: %q is not a table name (schema.table)", ErrInvalid, t)
		}
	}
	return nil
}

// UpdateSettings changes CORS origins and gateway settings.
func (s *Service) UpdateSettings(ctx context.Context, projectID uuid.UUID, origins []string, st Settings) (store.ProjectService, error) {
	cur, err := store.New(s.db).GetProjectServices(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return cur, fmt.Errorf("%w: backend services were never enabled", ErrNotFound)
	} else if err != nil {
		return cur, err
	}
	return s.UpdateExposure(ctx, projectID, origins, st, cur.ExposedSchemas, cur.PublicTables)
}

// UpdateExposure changes CORS origins, gateway settings, the exposed
// schemas and the tables anon and user may read without row-level
// security. A newly exposed schema gets the request roles' grants.
func (s *Service) UpdateExposure(ctx context.Context, projectID uuid.UUID, origins []string, st Settings, schemas, public []string) (store.ProjectService, error) {
	if err := st.Validate(); err != nil {
		return store.ProjectService{}, err
	}
	if err := validExposure(schemas, public); err != nil {
		return store.ProjectService{}, err
	}
	clean := []string{}
	for _, o := range origins {
		o = strings.TrimRight(strings.TrimSpace(o), "/")
		if o == "" {
			continue
		}
		web := strings.HasPrefix(o, "https://") || strings.HasPrefix(o, "http://")
		if (o != "*" && !web) || strings.ContainsAny(o, " ,") {
			return store.ProjectService{}, fmt.Errorf("%w: %q is not an origin (https://app.example.com)", ErrInvalid, o)
		}
		clean = append(clean, o)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return store.ProjectService{}, err
	}
	q := store.New(s.db)
	svc, err := q.GetProjectServices(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return svc, fmt.Errorf("%w: backend services were never enabled", ErrNotFound)
	} else if err != nil {
		return svc, err
	}
	if svc.Enabled && !sameSet(svc.ExposedSchemas, schemas) {
		p, err := q.GetProject(ctx, projectID)
		if err != nil {
			return svc, err
		}
		if err := s.applyExposure(ctx, p, schemas); err != nil {
			return svc, err
		}
	}
	return q.UpdateServicesSettings(ctx, store.UpdateServicesSettingsParams{ProjectID: projectID, CorsOrigins: clean, Settings: raw,
		ExposedSchemas: schemas, PublicTables: public})
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

// applyExposure grants the request roles the exposed schemas besides
// public; a schema that doesn't exist is refused.
func (s *Service) applyExposure(ctx context.Context, p store.Project, schemas []string) error {
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		for _, sc := range schemas {
			if sc == "public" {
				continue
			}
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, sc).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: there is no schema %q", ErrInvalid, sc)
			}
			for _, st := range exposureStmts {
				if _, err := tx.Exec(ctx, strings.ReplaceAll(expand(st, p), "{{schema}}", provision.Ident(sc))); err != nil {
					return fmt.Errorf("expose %s: %w", sc, err)
				}
			}
		}
		return nil
	})
}

// ---- Operations -------------------------------------------------------------

func (s *Service) runEnable(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	q := store.New(s.db)
	p, err := q.GetProject(ctx, *op.ProjectID)
	if err != nil {
		return err
	}
	if p.DeletedAt != nil {
		return jobs.Permanent(fmt.Errorf("the project was deleted"))
	}
	return s.enableOn(ctx, p, log)
}

// enableOn makes p's roles, xb_* schemas and signing key and turns its
// API on.
func (s *Service) enableOn(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	q := store.New(s.db)
	if err := s.ensureRoles(ctx, p); err != nil {
		return fmt.Errorf("roles: %w", err)
	}
	if err := log.Info(ctx, "roles", "roles %s ready", strings.Join(store.ServiceRoles(p.DbName), ", ")); err != nil {
		return err
	}
	if err := s.applySchema(ctx, p, true); err != nil {
		return fmt.Errorf("xb schemas: %w", err)
	}
	if err := log.Info(ctx, "schema", "xb_auth, xb_storage and xb_realtime at version %d", SchemaVersion); err != nil {
		return err
	}
	if _, err := s.ensureJWTKey(ctx, p.ID); err != nil {
		return fmt.Errorf("signing key: %w", err)
	}
	if err := log.Info(ctx, "keys", "ES256 signing key ready"); err != nil {
		return err
	}
	svc, err := q.SetServicesEnabled(ctx, store.SetServicesEnabledParams{ProjectID: p.ID, Enabled: true})
	if err != nil {
		return err
	}
	if err := s.projects.SyncPooler(ctx, log, "pooler", "edge login added to the poolers"); err != nil {
		return err
	}
	return log.Info(ctx, "enabled", "serving at %s", s.URL(svc.Ref, p.Region))
}

func (s *Service) runDisable(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	q := store.New(s.db)
	p, err := q.GetProject(ctx, *op.ProjectID)
	if err != nil {
		return err
	}
	if err := s.projects.SyncPooler(ctx, log, "pooler", "edge login removed from the poolers"); err != nil {
		return err
	}
	if p.DeletedAt == nil {
		conn, err := s.projects.AdminConn(ctx, p.InstanceID, "postgres")
		if err != nil {
			return err
		}
		defer conn.Close(context.Background())
		for _, role := range store.ServiceRoles(p.DbName) {
			var exists bool
			if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				continue
			}
			if _, err := conn.Exec(ctx, "ALTER ROLE "+provision.Ident(role)+" NOLOGIN"); err != nil {
				return err
			}
			if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1`, role); err != nil {
				return err
			}
		}
	}
	return log.Info(ctx, "disabled", "keys revoked and the edge's logins switched off; the xb_* schemas are kept")
}

// edgePassword is the edge login's password, derived from the master key,
// so it is never stored.
func (s *Service) edgePassword(role string) string {
	return base64.RawURLEncoding.EncodeToString(s.keyring.Derive("xenibase edge role "+role, 32))
}

// ensureRoles creates or repairs the four roles on p's instance (V4 §2.3)
// and records it.
func (s *Service) ensureRoles(ctx context.Context, p store.Project) error {
	return s.ensureRolesOn(ctx, p, p.InstanceID, true)
}

// EnsureRolesOn creates p's roles on another instance, if p has backend
// services: a promotion, demotion or move copies grants to them, which
// fail without them (the dedicated service calls it before copying).
func (s *Service) EnsureRolesOn(ctx context.Context, p store.Project, instance uuid.UUID) error {
	if _, err := store.New(s.db).GetProjectServices(ctx, p.ID); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	return s.ensureRolesOn(ctx, p, instance, false)
}

// MarkForReconcile has the reconciler re-apply p's roles and every schema
// version (its database was replaced: a restore in place, a branch reset).
func (s *Service) MarkForReconcile(ctx context.Context, p store.Project) {
	if _, err := s.db.Exec(ctx, `UPDATE project_services SET roles_instance = NULL WHERE project_id = $1 AND roles_instance IS NOT NULL`, p.ID); err != nil {
		s.log.Warn("backend services: mark for reconcile", "project_id", p.ID, "err", err)
	}
}

func (s *Service) ensureRolesOn(ctx context.Context, p store.Project, instance uuid.UUID, record bool) error {
	q := store.New(s.db)
	svc, err := q.GetProjectServices(ctx, p.ID)
	if err != nil {
		return err
	}
	edge := store.EdgeRole(p.DbName)
	verifier := ""
	if svc.EdgeVerifier != nil {
		verifier = *svc.EdgeVerifier
	} else if verifier, err = crypto.SCRAMVerifier(s.edgePassword(edge)); err != nil {
		return err
	}
	// The request roles' own logins (V4-M32): their stored verifiers, or new
	// ones from their derived passwords.
	logins := map[string]string{}
	if len(svc.LoginVerifiers) > 0 {
		if err := json.Unmarshal(svc.LoginVerifiers, &logins); err != nil {
			return fmt.Errorf("login verifiers: %w", err)
		}
	}
	for _, r := range store.RequestRoles(p.DbName) {
		if logins[r] == "" {
			v, err := crypto.SCRAMVerifier(s.edgePassword(r))
			if err != nil {
				return err
			}
			logins[r] = v
		}
	}
	conn, err := s.projects.AdminConn(ctx, instance, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	id := provision.Ident
	create := func(role, attrs string) error {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
			return err
		}
		verb := "CREATE"
		if exists {
			verb = "ALTER"
		}
		_, err := conn.Exec(ctx, verb+" ROLE "+id(role)+" "+attrs)
		var pe *pgconn.PgError
		if verb == "CREATE" && errors.As(err, &pe) && pe.Code == "42710" {
			_, err = conn.Exec(ctx, "ALTER ROLE "+id(role)+" "+attrs)
		}
		return err
	}
	const none = "NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION"
	login := func(role string, limit int) string {
		return fmt.Sprintf("LOGIN NOINHERIT %s CONNECTION LIMIT %d PASSWORD '%s'", none, limit, logins[role])
	}
	for _, r := range []struct{ role, attrs string }{
		{store.AnonRole(p.DbName), "NOBYPASSRLS " + login(store.AnonRole(p.DbName), requestConnLimit)},
		{store.UserRole(p.DbName), "NOBYPASSRLS " + login(store.UserRole(p.DbName), requestConnLimit)},
		// Bypasses row-level security, still not a superuser.
		{store.ServiceRole(p.DbName), "BYPASSRLS " + login(store.ServiceRole(p.DbName), requestConnLimit)},
		{store.AuthHookRole(p.DbName), "NOBYPASSRLS " + login(store.AuthHookRole(p.DbName), hookConnLimit)},
		// The edge's own login: the auth tables, and nothing it runs is the
		// tenant's.
		{edge, fmt.Sprintf("LOGIN NOINHERIT NOBYPASSRLS %s CONNECTION LIMIT %d PASSWORD '%s'", none, edgeConnLimit, verifier)},
	} {
		if err := create(r.role, r.attrs); err != nil {
			return fmt.Errorf("%s: %w", r.role, err)
		}
	}
	// Before M32 the edge login could SET ROLE to the request roles; tenant
	// SQL reset to it could then read xb_auth or become the service role.
	var member bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.member
		WHERE r.rolname = $1)`, edge).Scan(&member); err != nil {
		return err
	}
	var stmts []string
	if member {
		for _, r := range store.RequestRoles(p.DbName) {
			stmts = append(stmts, "REVOKE "+id(r)+" FROM "+id(edge))
		}
	}
	for _, r := range append(store.RequestRoles(p.DbName), edge) {
		stmts = append(stmts, "GRANT CONNECT ON DATABASE "+id(p.DbName)+" TO "+id(r))
		if p.Tier == provision.TierShared {
			stmts = append(stmts, "ALTER ROLE "+id(r)+" SET temp_file_limit = '"+provision.TempFileLimit+"'")
		}
	}
	for _, st := range stmts {
		if _, err := conn.Exec(ctx, st); err != nil {
			var pe *pgconn.PgError
			if strings.HasPrefix(st, "REVOKE") && errors.As(err, &pe) {
				continue // not a member on this instance
			}
			return fmt.Errorf("%s: %w", st, err)
		}
	}
	if !record {
		return nil
	}
	lv, _ := json.Marshal(logins)
	return q.SetServicesRoles(ctx, store.SetServicesRolesParams{ProjectID: p.ID, EdgeVerifier: &verifier, LoginVerifiers: lv,
		SchemaVersion: svc.SchemaVersion, RolesInstance: &p.InstanceID})
}

// applySchema brings p's xb_* schemas to SchemaVersion; with all, it
// re-runs every version (after a move, so grants to the roles hold).
func (s *Service) applySchema(ctx context.Context, p store.Project, all bool) error {
	q := store.New(s.db)
	svc, err := q.GetProjectServices(ctx, p.ID)
	if err != nil {
		return err
	}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	from := int(svc.SchemaVersion)
	if all {
		from = 0
	}
	if all {
		// A restore by the project's owner (a backup, a branch) recreates the
		// xb_* schemas as the owner's: take them back.
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, reownSQL)
			return err
		}); err != nil {
			return fmt.Errorf("re-own the xb schemas: %w", err)
		}
	}
	for _, v := range schemaVersions {
		if v.n <= from {
			continue
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			for _, st := range v.stmts {
				if _, err := tx.Exec(ctx, expand(st, p)); err != nil {
					return fmt.Errorf("version %d: %w", v.n, err)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if all {
		if err := s.applyExposure(ctx, p, svc.ExposedSchemas); err != nil {
			return err
		}
	}
	return q.SetServicesRoles(ctx, store.SetServicesRolesParams{ProjectID: p.ID, EdgeVerifier: svc.EdgeVerifier, LoginVerifiers: svc.LoginVerifiers,
		SchemaVersion: int32(SchemaVersion), RolesInstance: svc.RolesInstance})
}

// Reconcile re-applies roles and schemas for enabled projects that moved
// to another instance or whose schemas are behind this release.
func (s *Service) Reconcile(ctx context.Context) error {
	ps, err := store.New(s.db).ServicesToReconcile(ctx, int32(SchemaVersion))
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range ps {
		svc, err := store.New(s.db).GetProjectServices(ctx, p.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		moved := svc.RolesInstance == nil || *svc.RolesInstance != p.InstanceID
		// A newer schema version may use a role this release added.
		if moved || svc.SchemaVersion < int32(SchemaVersion) {
			if err := s.ensureRoles(ctx, p); err != nil {
				errs = append(errs, fmt.Errorf("%s roles: %w", p.DbName, err))
				continue
			}
		}
		if err := s.applySchema(ctx, p, moved); err != nil {
			errs = append(errs, fmt.Errorf("%s schema: %w", p.DbName, err))
			continue
		}
		s.log.Info("backend services roles reconciled", "project_id", p.ID, "moved", moved)
	}
	return errors.Join(errs...)
}

// Run reconciles and prunes request logs every interval.
func (s *Service) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	var swept, reconciled time.Time
	for {
		if time.Since(swept) >= StorageSweepEvery {
			if err := s.StorageSweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("storage sweep", "err", err)
			}
			if err := s.RealtimeSweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("realtime sweep", "err", err)
			}
			if err := s.PlanLimitsSweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("plan limits sweep", "err", err)
			}
			swept = time.Now()
		}
		if time.Since(reconciled) >= ReconcileStorageEvery {
			// The first run waits a sweep, so a restart doesn't reconcile.
			if !reconciled.IsZero() {
				if err := s.ReconcileStorage(ctx); err != nil && ctx.Err() == nil {
					s.log.Warn("storage reconcile", "err", err)
				}
			}
			reconciled = time.Now()
		}
		if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("backend services reconcile", "err", err)
		}
		q := store.New(s.db)
		if _, err := q.PruneRequestLogs(ctx, time.Now().Add(-7*24*time.Hour)); err != nil && ctx.Err() == nil {
			s.log.Warn("pruning API request logs", "err", err)
		}
		if _, err := q.PruneEdgeReports(ctx, time.Now().Add(-7*24*time.Hour)); err != nil && ctx.Err() == nil {
			s.log.Warn("pruning edge reports", "err", err)
		}
		if _, err := q.RetireJWTKeys(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("retiring signing keys", "err", err)
		}
		if _, err := q.PruneAuthEmails(ctx, time.Now().Add(-7*24*time.Hour)); err != nil && ctx.Err() == nil {
			s.log.Warn("pruning auth emails", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- Signing keys -----------------------------------------------------------

func jwtAAD(keyID uuid.UUID) []byte { return []byte("project_jwt_keys.private_enc:" + keyID.String()) }

// JWTKeyAAD is the associated data a signing key's private half is sealed
// with (for master-key rotation).
func JWTKeyAAD(keyID uuid.UUID) []byte { return jwtAAD(keyID) }

func (s *Service) ensureJWTKey(ctx context.Context, projectID uuid.UUID) (store.ProjectJwtKey, error) {
	q := store.New(s.db)
	k, err := q.ActiveJWTKey(ctx, projectID)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return k, err
	}
	kid, err := randomFrom(refRest, 16)
	if err != nil {
		return k, err
	}
	der, jwk, err := jwtes.Generate(kid)
	if err != nil {
		return k, err
	}
	pub, err := json.Marshal(jwk)
	if err != nil {
		return k, err
	}
	// The row's id is the sealing context, so make it first.
	id := uuid.New()
	sealed, err := s.keyring.Encrypt(der, jwtAAD(id))
	if err != nil {
		return k, err
	}
	return q.InsertJWTKey(ctx, store.InsertJWTKeyParams{ID: id, ProjectID: projectID, Kid: kid, PublicJwk: pub, PrivateEnc: sealed})
}

// MintToken signs an access token for p with its active key: the auth
// service's (V4 §4.4), and tests'. claims gets aud, iat and exp added.
func (s *Service) MintToken(ctx context.Context, projectID uuid.UUID, claims map[string]any, ttl time.Duration) (string, error) {
	q := store.New(s.db)
	svc, err := q.GetProjectServices(ctx, projectID)
	if err != nil {
		return "", err
	}
	k, err := q.ActiveJWTKey(ctx, projectID)
	if err != nil {
		return "", err
	}
	der, err := s.keyring.Decrypt(k.PrivateEnc, jwtAAD(k.ID))
	if err != nil {
		return "", err
	}
	now := time.Now()
	c := map[string]any{}
	for k, v := range claims {
		c[k] = v
	}
	c["aud"], c["iat"], c["exp"] = svc.Ref, now.Unix(), now.Add(ttl).Unix()
	return jwtes.Sign(der, k.Kid, c)
}
