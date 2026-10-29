// Package dedicated runs the dedicated tier (spec §4.2): one Postgres
// container per project, created on a node by its agent, with WAL-G
// continuous archiving, daily base backups, and point-in-time recovery into
// a new project (§6.4, §6.5).
package dedicated

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/ha"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/walg"
)

// Secrets supplies backup storage and keys (the backup service).
type Secrets interface {
	// StorageTarget is the platform default target.
	StorageTarget(ctx context.Context) (uuid.UUID, storage.Target, error)
	// BackupKey is the instance backup key.
	BackupKey(ctx context.Context) ([]byte, error)
	// TargetByID opens any target (platform or org).
	TargetByID(ctx context.Context, id uuid.UUID) (storage.Target, error)
	// ProjectPGPKey is the OpenPGP key of a per-project backup key.
	ProjectPGPKey(ctx context.Context, keyID uuid.UUID) (string, error)
	// ProjectWALG is where a project's archive should be now and with which
	// key (nil: the instance key), V2 s6.
	ProjectWALG(ctx context.Context, p store.Project) (uuid.UUID, *uuid.UUID, error)
}

// Config tunes the tier.
type Config struct {
	// AdminVia is how the control plane reaches instances: "network" (the
	// host the agent reports, e.g. a container name on a shared Docker
	// network; the default) or "published" (the port the agent publishes on
	// the node).
	AdminVia string
	// RetainFull is how many full base backups WAL-G keeps (spec §6.4).
	RetainFull int
	// HostPoll is how often a creation waiting for a new host checks it
	// (5 s by default).
	HostPoll time.Duration
	// ReadyTimeout bounds waiting for a restored instance to promote.
	ReadyTimeout time.Duration
	// AfterFreeze, if set, runs during a promotion or demotion right after
	// writes are frozen; an error fails it there (tests use it to exercise
	// the rollback).
	AfterFreeze func(ctx context.Context) error
	// MoveWait tunes when a logical move cuts over (defaults: lag under
	// 1 MB for 30 s, checked every 2 s).
	MoveWait MoveWait
	// RequireAnnouncement: the maintenance window restarts an HA project
	// only inside maintenance announced to it at least 72 hours before
	// (V3.1 §4.3; XENIBASE_MAINTENANCE_REQUIRE_ANNOUNCEMENT).
	RequireAnnouncement bool
	// ReplicaMaxLag is how far behind a read replica may fall before it
	// leaves the pooler's read-only route (V4 §7; default 10 s).
	ReplicaMaxLag time.Duration
}

// Service manages dedicated instances.
type Service struct {
	db       *pgxpool.Pool
	keyring  *crypto.Keyring
	nodes    *nodes.Service
	projects *provision.Service
	secrets  Secrets
	cfg      Config
	log      *slog.Logger

	// Etcd is the etcd cluster HA instances use (V3 §2.2); nil when the
	// server runs without one.
	Etcd  *ha.Service
	watch watcher
	sla   slaState
	// Quotas, when set, checks organisation limits for demotions (the
	// tenancy service).
	Quotas Quotas
	// Snapshot, when set, takes a logical backup of a project (the backup
	// service): a demoted project's first (V2 §5.3 step 7).
	Snapshot func(ctx context.Context, p store.Project, log *jobs.StepLogger) error
	// ServiceRoles, when set, creates a project's backend services roles on
	// another instance before its database is copied there (V4 §2.5).
	ServiceRoles func(ctx context.Context, p store.Project, instance uuid.UUID) error
	// Hosts, when set, provides a new dedicated host when no node has room
	// (the capacity service, V4.1 §5.3).
	Hosts HostFunc
	// proposer drafts the announcements the HA window gate waits for.
	proposer Proposer
	// etcdMoveOne replaces MoveToRegionEtcd in Move all (tests).
	etcdMoveOne func(ctx context.Context, project uuid.UUID, by *uuid.UUID) (store.Operation, error)
}

// New returns a Service.
func New(db *pgxpool.Pool, keyring *crypto.Keyring, ns *nodes.Service, ps *provision.Service, secrets Secrets, cfg Config, log *slog.Logger) *Service {
	if cfg.AdminVia == "" {
		cfg.AdminVia = "network"
	}
	if cfg.RetainFull <= 0 {
		cfg.RetainFull = 7
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 15 * time.Minute
	}
	if cfg.ReplicaMaxLag <= 0 {
		cfg.ReplicaMaxLag = DefaultReplicaMaxLag
	}
	return &Service{db: db, keyring: keyring, nodes: ns, projects: ps, secrets: secrets, cfg: cfg, log: log}
}

var _ provision.InstanceManager = (*Service)(nil)

// Profiles are the instance sizes on offer.
var Profiles = []provision.Profile{
	{Name: "small", CPUs: 1, MemoryMB: 1024},
	{Name: "medium", CPUs: 2, MemoryMB: 4096},
	{Name: "large", CPUs: 4, MemoryMB: 8192},
}

// DefaultProfile and DefaultVolumeGB apply when a request names none.
const (
	DefaultProfile  = "small"
	DefaultVolumeGB = 20
)

// ProfileByName finds a profile.
func ProfileByName(name string) (provision.Profile, bool) {
	for _, p := range Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return provision.Profile{}, false
}

// settings is postgresql.conf for a profile (spec §4.2: its own tuning)
// with maxConnections connections.
func settings(p provision.Profile, maxConnections int) map[string]string {
	mb := func(n int) string { return strconv.Itoa(n) + "MB" }
	return map[string]string{
		"max_connections":            strconv.Itoa(maxConnections),
		"shared_buffers":             mb(p.MemoryMB / 4),
		"effective_cache_size":       mb(p.MemoryMB * 3 / 4),
		"maintenance_work_mem":       mb(min(max(p.MemoryMB/16, 64), 2048)),
		"work_mem":                   "8MB",
		"wal_compression":            "on",
		"checkpoint_timeout":         "15min",
		"max_wal_size":               "2GB",
		"shared_preload_libraries":   "pg_stat_statements",
		"log_min_duration_statement": "5s",
	}
}

// Validate implements provision.InstanceManager.
func (s *Service) Validate(ctx context.Context, p *provision.CreateParams) (provision.Profile, error) {
	var prof provision.Profile
	if p.Size != nil {
		// A size given outright (a resized source's), named as the
		// instance row will be.
		if p.Size.CPUs <= 0 || p.Size.MemoryMB <= 0 {
			return prof, fmt.Errorf("%w: an instance needs CPUs and memory", provision.ErrInvalid)
		}
		prof = provision.Profile{Name: profileName(Size{CPUs: p.Size.CPUs, MemoryMB: p.Size.MemoryMB}), CPUs: p.Size.CPUs, MemoryMB: p.Size.MemoryMB}
		p.Profile = prof.Name
	} else {
		if p.Profile == "" {
			p.Profile = DefaultProfile
		}
		var ok bool
		if prof, ok = ProfileByName(p.Profile); !ok {
			return prof, fmt.Errorf("%w: unknown profile %q", provision.ErrInvalid, p.Profile)
		}
	}
	if p.VolumeGB == 0 {
		p.VolumeGB = DefaultVolumeGB
	}
	if p.VolumeGB < 1 || p.VolumeGB > 16384 {
		return prof, fmt.Errorf("%w: volume must be 1 to 16384 GB", provision.ErrInvalid)
	}
	// WAL-G needs somewhere to archive to from the first second.
	if _, _, err := s.secrets.StorageTarget(ctx); err != nil {
		return prof, fmt.Errorf("%w: dedicated projects archive WAL continuously: %w", provision.ErrConflict, err)
	}
	if _, err := s.secrets.BackupKey(ctx); err != nil {
		return prof, fmt.Errorf("%w: dedicated projects archive WAL continuously: %w", provision.ErrConflict, err)
	}
	q := store.New(s.db)
	if p.NodeID == nil {
		id, err := s.placeDedicated(ctx, q, p.Region, Size{CPUs: prof.CPUs, MemoryMB: prof.MemoryMB, DiskGB: p.VolumeGB})
		if err != nil {
			return prof, err
		}
		p.NodeID = &id
		return prof, nil
	}
	n, err := q.GetNode(ctx, *p.NodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return prof, fmt.Errorf("%w: no such node", provision.ErrInvalid)
	}
	if err != nil {
		return prof, err
	}
	if n.Role != "dedicated" && n.Role != "both" {
		return prof, fmt.Errorf("%w: node %s does not take dedicated instances (role %s)", provision.ErrInvalid, n.Name, n.Role)
	}
	if n.AgentCertFp == nil {
		return prof, fmt.Errorf("%w: node %s has no agent", provision.ErrConflict, n.Name)
	}
	// Placement by load already skips unhealthy nodes (PickDedicatedNode); a
	// node chosen by hand gets the same answer now instead of a create that
	// retries against a dead agent and fails.
	if n.Status != "healthy" {
		return prof, fmt.Errorf("%w: node %s is %s; its agent has not answered recently", provision.ErrConflict, n.Name, n.Status)
	}
	// And the rest of placeDedicated's checks: in service, with room. A
	// detached replica's instance is already running there.
	if p.InstanceID != nil {
		return prof, nil
	}
	if n.Lifecycle != "active" {
		return prof, fmt.Errorf("%w: node %s is %s and takes no new instances", provision.ErrConflict, n.Name, n.Lifecycle)
	}
	nodes, err := q.CapacityNodes(ctx)
	if err != nil {
		return prof, err
	}
	for _, c := range nodes {
		if c.ID != n.ID {
			continue
		}
		if ok, why := fits(c, prof.CPUs, int64(prof.MemoryMB), int64(p.VolumeGB)); !ok {
			return prof, fmt.Errorf("%w: %s", provision.ErrNoCapacity, why)
		}
	}
	return prof, nil
}

// pitrParams is the "pitr" entry of a restore operation's params.
type pitrParams struct {
	SourceProject  uuid.UUID `json:"source_project"`
	SourceInstance uuid.UUID `json:"source_instance"`
	BackupID       uuid.UUID `json:"backup_id"`
	BackupName     string    `json:"backup_name"`
	TargetTime     string    `json:"target_time,omitempty"`
}

func opPITR(op store.Operation) (*pitrParams, error) {
	var p struct {
		PITR *pitrParams `json:"pitr"`
	}
	if len(op.Params) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(op.Params, &p); err != nil {
		return nil, jobs.Permanent(err)
	}
	return p.PITR, nil
}

// walgFor is where an instance archives: its walg_prefix on its WAL-G
// target, with its WAL-G key.
func (s *Service) walgFor(ctx context.Context, inst store.Instance) (agentapi.WALG, error) {
	if inst.WalgPrefix == nil {
		return agentapi.WALG{}, fmt.Errorf("instance %s has no WAL-G prefix", inst.ID)
	}
	return s.walgAt(ctx, inst.WalgTargetID, inst.WalgKeyID, *inst.WalgPrefix)
}

// walgAt is an archive's location and key. A nil target is the platform
// default; a nil key is the instance backup key.
func (s *Service) walgAt(ctx context.Context, targetID, keyID *uuid.UUID, prefix string) (agentapi.WALG, error) {
	var target storage.Target
	var err error
	if targetID == nil {
		_, target, err = s.secrets.StorageTarget(ctx)
	} else {
		target, err = s.secrets.TargetByID(ctx, *targetID)
	}
	if err != nil {
		return agentapi.WALG{}, jobs.Permanent(err)
	}
	var key string
	if keyID != nil {
		key, err = s.secrets.ProjectPGPKey(ctx, *keyID)
	} else {
		var bk []byte
		if bk, err = s.secrets.BackupKey(ctx); err != nil {
			return agentapi.WALG{}, jobs.Permanent(err)
		}
		key, err = walg.PGPKey(bk)
	}
	if err != nil {
		return agentapi.WALG{}, err
	}
	return agentapi.WALG{Storage: target, Prefix: prefix, PGPKey: key}, nil
}

// walgForBackup is where a base backup's archive is: the target, key, and
// prefix recorded with it (V2 s6: switching keeps old backups restorable).
func (s *Service) walgForBackup(ctx context.Context, b store.Backup, inst store.Instance) (agentapi.WALG, error) {
	if b.WalgPrefix == nil {
		return s.walgFor(ctx, inst)
	}
	return s.walgAt(ctx, b.StorageTargetID, b.EncryptionKeyID, *b.WalgPrefix)
}

// placeArchive records, for an instance that has never run, the target and
// key its project's backups use now.
func (s *Service) placeArchive(ctx context.Context, inst store.Instance, p store.Project) (store.Instance, error) {
	if inst.WalgTargetID != nil || inst.WalgPrefix == nil {
		return inst, nil
	}
	targetID, keyID, err := s.secrets.ProjectWALG(ctx, p)
	if err != nil {
		return inst, jobs.Permanent(err)
	}
	q := store.New(s.db)
	if err := q.SetInstanceWALG(ctx, store.SetInstanceWALGParams{ID: inst.ID, WalgPrefix: inst.WalgPrefix, WalgTargetID: &targetID, WalgKeyID: keyID}); err != nil {
		return inst, err
	}
	return q.GetInstance(ctx, inst.ID)
}

func randomPassword() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func profileOf(inst store.Instance) provision.Profile {
	p := provision.Profile{Name: DefaultProfile, CPUs: 1, MemoryMB: 1024}
	if inst.Profile != nil {
		if pp, ok := ProfileByName(*inst.Profile); ok {
			p = pp
		}
	}
	if inst.MemLimitMb != nil {
		p.MemoryMB = int(*inst.MemLimitMb)
	}
	if f, err := inst.CpuLimit.Float64Value(); err == nil && f.Valid {
		p.CPUs = f.Float64
	}
	return p
}

// Ensure implements provision.InstanceManager: it creates the project's
// container (restoring it for a point-in-time recovery), records how to
// reach it, and hardens it.
func (s *Service) Ensure(ctx context.Context, op store.Operation, p store.Project, log *jobs.StepLogger) error {
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, p.InstanceID)
	if err != nil {
		return err
	}
	if inst.DeletedAt != nil {
		// Never a new container under a removed instance's row: every
		// query that skips deleted instances would lose sight of it.
		return jobs.Permanent(fmt.Errorf("instance %s was removed", inst.ID))
	}
	pitr, err := opPITR(op)
	if err != nil {
		return err
	}
	if d, err := opDetach(op); err != nil {
		return err
	} else if d != nil {
		return s.ensureDetached(ctx, inst, p, d, log)
	}
	if inst.Status != "running" {
		if err := s.waitForHost(ctx, inst.NodeID, log); err != nil {
			return err
		}
	}
	if inst.Status == "running" {
		if err := s.adoptRestore(ctx, inst, p, pitr, log); err != nil {
			return err
		}
		return log.Info(ctx, "instance", "instance already running")
	}

	// The superuser: generated, or for a restore the source's (it is in the
	// restored data).
	if len(inst.AdminSecret) == 0 {
		secret := provision.AdminSecret{User: "xenibase_admin", Password: randomPassword()}
		if pitr != nil {
			src, err := q.GetInstance(ctx, pitr.SourceInstance)
			if err != nil {
				return jobs.Permanent(fmt.Errorf("source instance: %w", err))
			}
			if secret, err = provision.OpenInstanceSecret(s.keyring, src.ID, src.AdminSecret); err != nil {
				return jobs.Permanent(err)
			}
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
	if inst, err = s.placeArchive(ctx, inst, p); err != nil {
		return err
	}
	spec, err := s.instanceSpec(ctx, inst)
	if err != nil {
		return err
	}
	w, prof := *spec.WALG, profileOf(inst)
	if pitr != nil {
		src, err := q.GetInstance(ctx, pitr.SourceInstance)
		if err != nil {
			return jobs.Permanent(fmt.Errorf("source instance: %w", err))
		}
		srcBackup, err := q.GetBackup(ctx, pitr.BackupID)
		if err != nil {
			return jobs.Permanent(fmt.Errorf("source base backup: %w", err))
		}
		srcWALG, err := s.walgForBackup(ctx, srcBackup, src)
		if err != nil {
			return err
		}
		spec.Restore = &agentapi.WALGRestore{Source: srcWALG, BackupName: pitr.BackupName, TargetTime: pitr.TargetTime}
		target := "the end of the archive"
		if pitr.TargetTime != "" {
			target = pitr.TargetTime
		}
		if err := log.Info(ctx, "restore", "fetching base backup %s and replaying WAL to %s", pitr.BackupName, target); err != nil {
			return err
		}
	}
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return err
	}
	if err := log.Info(ctx, "instance", "starting a %s instance (%g CPU, %d MB) on node %s", prof.Name, prof.CPUs, prof.MemoryMB, agent.Node.Name); err != nil {
		return err
	}
	start := time.Now()
	res, err := agent.CreateInstance(ctx, spec)
	if err != nil {
		_ = q.SetInstanceStatus(context.WithoutCancel(ctx), store.SetInstanceStatusParams{ID: inst.ID, Status: "provisioning", Error: ptr(err.Error())})
		return err
	}
	if res.Host == "" {
		return jobs.Permanent(fmt.Errorf("agent on %s reports no address for the instance (set XENIBASE_AGENT_NETWORK or XENIBASE_AGENT_PUBLISH)", agent.Node.Name))
	}
	if err := s.recordRunning(ctx, inst, agent, res); err != nil {
		return err
	}
	if err := log.Info(ctx, "instance", "container %s up in %s, archiving WAL to %s", res.Container,
		time.Since(start).Round(time.Second), walg.S3Prefix(w)); err != nil {
		return err
	}
	inst, err = q.GetInstance(ctx, inst.ID)
	if err != nil {
		return err
	}
	return s.adoptRestore(ctx, inst, p, pitr, log)
}

func ptr[T any](v T) *T { return &v }

// instanceSpec is the container spec of an instance whose admin secret is
// set.
func (s *Service) instanceSpec(ctx context.Context, inst store.Instance) (agentapi.InstanceSpec, error) {
	secret, err := provision.OpenInstanceSecret(s.keyring, inst.ID, inst.AdminSecret)
	if err != nil {
		return agentapi.InstanceSpec{}, jobs.Permanent(err)
	}
	if inst.Kind == provision.TierShared {
		return sharedSpec(inst, secret), nil
	}
	if inst.Patroni {
		// An HA instance: the leader's member (V3 §2.2).
		return s.memberSpec(ctx, inst, leaderKey(inst))
	}
	w, err := s.walgFor(ctx, inst)
	if err != nil {
		return agentapi.InstanceSpec{}, err
	}
	prof := profileOf(inst)
	return agentapi.InstanceSpec{
		ID: inst.ID.String(), Kind: agentapi.InstanceDedicated, CPUs: prof.CPUs, MemoryMB: prof.MemoryMB,
		AdminUser: secret.User, AdminPassword: secret.Password, Settings: settings(prof, int(inst.MaxConnections)), WALG: &w,
		PGVersion: int(inst.PgVersion),
	}, nil
}

// recordRunning stores how to reach a started instance.
func (s *Service) recordRunning(ctx context.Context, inst store.Instance, agent *nodes.Agent, res agentapi.Instance) error {
	run := store.SetInstanceRunningParams{ID: inst.ID, ContainerID: &res.ContainerID, Host: &res.Host, Port: int32(res.Port)}
	if s.cfg.AdminVia == "published" && res.PublishedPort > 0 {
		host := res.PublishedHost
		if host == "" || host == "0.0.0.0" {
			host = agent.Node.PrivateAddr
		}
		port := int32(res.PublishedPort)
		run.AdminHost, run.AdminPort = &host, &port
	}
	return store.New(s.db).SetInstanceRunning(ctx, run)
}

// Recreate replaces an instance's container with one from the node's
// current image, on the same volume (spec §11.3: Postgres minor upgrades
// by restarting from the UI), then updates the pooler route if its
// address changed.
func (s *Service) Recreate(ctx context.Context, inst store.Instance) (agentapi.Instance, error) {
	spec, err := s.instanceSpec(ctx, inst)
	if err != nil {
		return agentapi.Instance{}, err
	}
	spec.Recreate = true
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return agentapi.Instance{}, err
	}
	res, err := agent.CreateInstance(ctx, spec)
	if err != nil {
		return res, err
	}
	if err := s.recordRunning(ctx, inst, agent, res); err != nil {
		return res, err
	}
	if inst.Host == nil || *inst.Host != res.Host || int(inst.Port) != res.Port {
		if err := s.projects.SyncPooler(ctx, nil, "pooler", "instance address changed"); err != nil {
			return res, err
		}
	}
	return res, nil
}

// adminConn connects to database on inst as its superuser.
func (s *Service) adminConn(ctx context.Context, inst store.Instance, database string) (*pgx.Conn, error) {
	t, err := store.New(s.db).GetInstanceTarget(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	secret, err := provision.OpenInstanceSecret(s.keyring, inst.ID, t.AdminSecret)
	if err != nil {
		return nil, err
	}
	host, port := t.Host, int(t.Port)
	if t.AdminHost != nil && *t.AdminHost != "" {
		host = *t.AdminHost
	}
	if t.AdminPort != nil && *t.AdminPort > 0 {
		port = int(*t.AdminPort)
	}
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%d sslmode=prefer", host, port))
	if err != nil {
		return nil, err
	}
	cfg.User, cfg.Password, cfg.Database = secret.User, secret.Password, database
	cfg.ConnectTimeout = 10 * time.Second
	cfg.RuntimeParams["application_name"] = "xenibase-server"
	return pgx.ConnectConfig(ctx, cfg)
}

// adoptRestore finishes a point-in-time recovery: wait for promotion,
// drop the recovery settings, and rename the source project's database
// and role to this project's (the password changes in Prepare). Then it
// hardens the cluster. Each step is idempotent.
func (s *Service) adoptRestore(ctx context.Context, inst store.Instance, p store.Project, pitr *pitrParams, log *jobs.StepLogger) error {
	conn, err := s.waitConn(ctx, inst)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if pitr != nil {
		deadline := time.Now().Add(s.cfg.ReadyTimeout)
		for {
			var inRecovery bool
			if err := conn.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil {
				return err
			}
			if !inRecovery {
				break
			}
			if time.Now().After(deadline) {
				return jobs.Permanent(errors.New("the restored instance did not finish recovery in time"))
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		for _, stmt := range []string{
			"ALTER SYSTEM RESET restore_command", "ALTER SYSTEM RESET recovery_end_command", "ALTER SYSTEM RESET recovery_target_time",
			"ALTER SYSTEM RESET recovery_target_action", "SELECT pg_reload_conf()",
		} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", stmt, err)
			}
		}
		src, err := store.New(s.db).GetProject(ctx, pitr.SourceProject)
		if err != nil {
			return err
		}
		// A recovery point from before the source was renamed to an opaque
		// name (or switched credentials) has its V1 names (V2 §10.2).
		srcDBs, srcOwners := []string{src.DbName}, []string{src.OwnerRole}
		if src.AliasDbName != nil {
			srcDBs = append(srcDBs, *src.AliasDbName)
		}
		if src.LegacyOwnerRole != nil {
			srcOwners = append(srcOwners, *src.LegacyOwnerRole)
		}
		for _, name := range srcDBs {
			if err := renameIfPresent(ctx, conn, "DATABASE", "pg_database", "datname", name, p.DbName); err != nil {
				return err
			}
		}
		for _, name := range srcOwners {
			if err := renameIfPresent(ctx, conn, "ROLE", "pg_roles", "rolname", name, p.OwnerRole); err != nil {
				return err
			}
		}
		// The source's console login came along in the base backup.
		for _, name := range srcDBs {
			if name != p.DbName {
				if err := provision.DropConsoleRole(ctx, conn, provision.ConsoleRole(name), p.DbName); err != nil {
					return err
				}
			}
		}
		if err := log.Info(ctx, "restore", "recovery finished and promoted; %s is now %s, owned by %s", src.DbName, p.DbName, p.OwnerRole); err != nil {
			return err
		}
		// The source's webhooks are not carried into the new project.
		if err := s.projects.ResetWebhooks(ctx, p, log); err != nil {
			return err
		}
	}
	for _, stmt := range []string{
		"REVOKE CONNECT, TEMPORARY ON DATABASE postgres FROM PUBLIC",
		"REVOKE CONNECT, TEMPORARY ON DATABASE template1 FROM PUBLIC",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("harden instance: %w", err)
		}
	}
	return nil
}

func renameIfPresent(ctx context.Context, conn *pgx.Conn, kind, catalog, column, from, to string) error {
	if from == to {
		return nil
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+catalog+" WHERE "+column+" = $1)", from).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil // renamed by an earlier attempt
	}
	if kind == "DATABASE" {
		if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, from); err != nil {
			return err
		}
	}
	_, err := conn.Exec(ctx, "ALTER "+kind+" "+provision.Ident(from)+" RENAME TO "+provision.Ident(to))
	return err
}

// waitConn connects to the instance, retrying while it starts.
func (s *Service) waitConn(ctx context.Context, inst store.Instance) (*pgx.Conn, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		conn, err := s.adminConn(ctx, inst, "postgres")
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connect to instance %s: %w", inst.ID, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Provisioned implements provision.InstanceManager: the first base backup.
func (s *Service) Provisioned(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	_, err := s.BaseBackup(ctx, p, nil, log)
	return err
}

// DefaultPITRDays is the point-in-time recovery window every dedicated
// instance has; 14 and 30 days are billed add-ons (V4.1 §4.1).
const DefaultPITRDays = 7

// RetainFull is how many daily base backups WAL-G keeps for inst: its
// window in days when longer than the default, else the configured count.
// WAL older than the oldest kept backup goes with it, so the window follows.
func (s *Service) RetainFull(inst store.Instance) int {
	if inst.PitrDays > DefaultPITRDays {
		return int(inst.PitrDays)
	}
	return s.cfg.RetainFull
}

// BaseBackup takes a WAL-G base backup of a dedicated project (spec §6.4),
// applies retention, and records it.
func (s *Service) BaseBackup(ctx context.Context, p store.Project, opID *uuid.UUID, log *jobs.StepLogger) (store.Backup, error) {
	if p.Tier != provision.TierDedicated {
		return store.Backup{}, jobs.Permanent(errors.New("base backups are for dedicated projects"))
	}
	q := store.New(s.db)
	inst, err := q.GetInstance(ctx, p.InstanceID)
	if err != nil {
		return store.Backup{}, err
	}
	targetID := inst.WalgTargetID
	if targetID == nil {
		id, _, err := s.secrets.StorageTarget(ctx)
		if err != nil {
			return store.Backup{}, jobs.Permanent(err)
		}
		targetID = &id
	}
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return store.Backup{}, err
	}
	if err := log.Info(ctx, "backup", "wal-g backup-push on %s", agent.Node.Name); err != nil {
		return store.Backup{}, err
	}
	// From the leader (V3 §2.2: WAL-G runs from the current primary).
	res, err := agent.BaseBackup(ctx, agentKey(inst), agentapi.WALGBackupRequest{RetainFull: s.RetainFull(inst)})
	if err != nil {
		return store.Backup{}, err
	}
	b := res.Backup
	size := b.CompressedSize
	row, err := q.InsertBaseBackup(ctx, store.InsertBaseBackupParams{
		ProjectID: &p.ID, ObjectKey: b.Name, StartedAt: b.StartTime, FinishedAt: &b.FinishTime,
		SizeBytes: &size, StorageTargetID: targetID, OperationID: opID,
		EncryptionKeyID: inst.WalgKeyID, WalgPrefix: inst.WalgPrefix,
	})
	if err != nil {
		return store.Backup{}, err
	}
	// Retention happened in WAL-G; forget backups it no longer lists.
	if list, err := agent.BaseBackups(ctx, agentKey(inst)); err == nil {
		keep := map[string]bool{}
		for _, l := range list {
			keep[l.Name] = true
		}
		rows, _ := q.ListBaseBackups(ctx, store.ListBaseBackupsParams{ProjectID: &p.ID, WalgPrefix: inst.WalgPrefix})
		for _, r := range rows {
			if !keep[r.ObjectKey] {
				_ = q.MarkBackupDeleted(ctx, r.ID)
			}
		}
	}
	return row, log.Info(ctx, "backup", "base backup %s: %s compressed (%s of data) in %s", b.Name,
		human(b.CompressedSize), human(b.UncompressedSize), (time.Duration(res.DurationMS) * time.Millisecond).Round(time.Millisecond))
}

func human(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	exp := int(math.Log(float64(n)) / math.Log(1024))
	exp = min(exp, 4)
	return fmt.Sprintf("%.1f %ciB", float64(n)/math.Pow(1024, float64(exp)), "KMGT"[exp-1])
}

// Destroy implements provision.InstanceManager: container, volume, and the
// WAL-G archive go. (The final logical backup is taken before this.)
func (s *Service) Destroy(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	inst, err := store.New(s.db).GetInstance(ctx, p.InstanceID)
	if err != nil {
		return err
	}
	return s.destroyInstance(ctx, inst, log)
}

// destroyInstance removes an instance's container, volume, and archive.
func (s *Service) destroyInstance(ctx context.Context, inst store.Instance, log *jobs.StepLogger) error {
	if inst.Status == "deleted" {
		return nil
	}
	node, archived, err := s.removeInstance(ctx, inst)
	if err != nil {
		return err
	}
	if archived.err != nil {
		_ = log.Warn(ctx, "drop", "could not delete the WAL-G archive: %v", archived.err)
	} else if archived.attempted {
		_ = log.Info(ctx, "drop", "deleted the WAL-G archive (%d objects)", archived.objects)
	}
	return log.Info(ctx, "drop", "removed container and volume on node %s", node)
}

// archiveResult reports what removing an instance's WAL-G archive did.
type archiveResult struct {
	attempted bool
	objects   int
	err       error
}

// removeInstance destroys an instance's container and volume on its node,
// deletes its WAL-G archive, and marks it deleted. Destroying what the node
// no longer has succeeds, so it is safe to repeat.
func (s *Service) removeInstance(ctx context.Context, inst store.Instance) (node string, archived archiveResult, err error) {
	agent, err := s.nodes.ForNode(ctx, inst.NodeID)
	if err != nil {
		return "", archived, err
	}
	// An HA instance's other members first, then the leader.
	members, err := store.New(s.db).ListInstanceMembers(ctx, inst.ID)
	if err != nil {
		return "", archived, err
	}
	for _, m := range members {
		if m.ID != leaderKey(inst) {
			if err := s.removeMember(ctx, m.ID, m.NodeID); err != nil {
				return "", archived, err
			}
		}
	}
	if err := store.New(s.db).DeleteInstanceReplicas(ctx, inst.ID); err != nil {
		return "", archived, err
	}
	if err := agent.DestroyInstance(ctx, agentKey(inst)); err != nil {
		return "", archived, err
	}
	if inst.WalgPrefix != nil {
		if w, err := s.walgFor(ctx, inst); err == nil {
			if c, err := storage.New(w.Storage); err == nil {
				archived.attempted = true
				archived.objects, archived.err = c.DeletePrefix(ctx, *inst.WalgPrefix)
			}
		}
	}
	if err := store.New(s.db).MarkInstanceDeleted(ctx, inst.ID); err != nil {
		return "", archived, err
	}
	return agent.Node.Name, archived, nil
}

// ReapOrphans removes instances left by a failed create whose rollback could
// not reach the node: dedicated instances no live project uses and no
// queued or running operation names as its target, and shared clusters
// marked "error" (spec §6.1: a failed create leaves nothing behind). An
// instance whose node is not healthy is left for a later pass. Each one is
// checked again just before it is destroyed, since the list can be minutes
// old by then. It returns how many were removed.
func (s *Service) ReapOrphans(ctx context.Context) (int, error) {
	q := store.New(s.db)
	orphans, err := q.ListOrphanedInstances(ctx, nil)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, inst := range orphans {
		n, err := q.GetNode(ctx, inst.NodeID)
		if err != nil {
			return removed, err
		}
		if n.Status != "healthy" {
			continue
		}
		if still, err := q.ListOrphanedInstances(ctx, &inst.ID); err != nil {
			return removed, err
		} else if len(still) == 0 {
			continue // taken up since the list was made
		}
		node, archived, err := s.removeInstance(ctx, inst)
		if err != nil {
			s.log.Warn("could not remove orphaned instance", "instance", inst.ID, "node", n.Name, "err", err)
			continue
		}
		if archived.err != nil {
			s.log.Warn("could not delete an orphaned instance's WAL-G archive", "instance", inst.ID, "err", archived.err)
		}
		s.log.Info("removed an instance left by a failed create", "instance", inst.ID, "node", node)
		removed++
	}
	return removed, nil
}

// RunReaper calls ReapOrphans and RepairStuckMoves (what failed rollbacks
// left) every interval until ctx ends.
func (s *Service) RunReaper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := s.ReapOrphans(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("orphaned instance cleanup failed", "err", err)
		}
		if _, err := s.RepairStuckMoves(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("repairing frozen projects failed", "err", err)
		}
	}
}
