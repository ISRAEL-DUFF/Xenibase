package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/store"
)

// Every step below is idempotent: a retried or reclaimed attempt re-runs
// the whole handler and converges on the same end state.

func (s *Service) project(ctx context.Context, op store.Operation) (store.Project, error) {
	if op.ProjectID == nil {
		return store.Project{}, jobs.Permanent(errors.New("operation has no project"))
	}
	p, err := store.New(s.db).GetProject(ctx, *op.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, jobs.Permanent(ErrNotFound)
	}
	return p, err
}

// runCreate implements spec §6.1 steps 3-6.
func (s *Service) runCreate(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	switch p.Status {
	case StatusActive:
		return log.Info(ctx, "done", "project is already active")
	case StatusProvisioning:
	default:
		return jobs.Permanent(fmt.Errorf("project is %s, not provisioning", p.Status))
	}
	var params createParams
	if err := json.Unmarshal(op.Params, &params); err != nil {
		return jobs.Permanent(fmt.Errorf("invalid params: %w", err))
	}
	password, err := s.openPassword(p.ID, KindCreate, params.Secrets)
	if err != nil {
		return jobs.Permanent(err)
	}
	settings, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		return jobs.Permanent(err)
	}

	start := time.Now()
	if err := s.EnsureInstance(ctx, op, p, log); err != nil {
		return err
	}
	if err := s.ensureRole(ctx, p, settings, log); err != nil {
		return err
	}
	if err := s.ensureDatabase(ctx, p, log); err != nil {
		return err
	}
	if err := s.hardenDatabase(ctx, p, log); err != nil {
		return err
	}
	if err := s.syncPooler(ctx, log, "pooler", "route and auth entry added"); err != nil {
		return err
	}
	if err := s.smokeTest(ctx, p, password, log); err != nil {
		return err
	}
	if err := store.New(s.db).SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: StatusActive}); err != nil {
		return err
	}
	if err := log.Info(ctx, "done", "project %s is active (provisioned in %s)", p.DbName, time.Since(start).Round(time.Millisecond)); err != nil {
		return err
	}
	s.Provisioned(ctx, p, log)
	return nil
}

// EnsureInstance creates a dedicated project's instance (a no-op for the
// shared tier).
func (s *Service) EnsureInstance(ctx context.Context, op store.Operation, p store.Project, log *jobs.StepLogger) error {
	if p.Tier != TierDedicated {
		return nil
	}
	if s.Instances == nil {
		return jobs.Permanent(ErrNoDedicated)
	}
	return s.Instances.Ensure(ctx, op, p, log)
}

// Provisioned runs the dedicated tier's after-create steps (the first base
// backup). A failure is only a warning: the project works, and the daily
// schedule takes the backup.
func (s *Service) Provisioned(ctx context.Context, p store.Project, log *jobs.StepLogger) {
	if p.Tier != TierDedicated || s.Instances == nil {
		return
	}
	if err := s.Instances.Provisioned(ctx, p, log); err != nil {
		_ = log.Warn(ctx, "backup", "first base backup failed: %v (the daily schedule will retry)", err)
	}
}

// OwnerCreateRole is the owner's CREATEROLE attribute: on for a dedicated
// project with role management on, where Postgres 16+ limits it to the
// roles the owner creates (the instance is the project's alone).
func OwnerCreateRole(p store.Project) string {
	if p.ManageRoles && p.Tier == TierDedicated {
		return "CREATEROLE"
	}
	return "NOCREATEROLE"
}

func (s *Service) ensureRole(ctx context.Context, p store.Project, set store.ProjectSettings, log *jobs.StepLogger) error {
	if !crypto.IsSCRAMVerifier(p.ScramVerifier) || strings.Contains(p.ScramVerifier, "'") {
		return jobs.Permanent(errors.New("project has an invalid SCRAM verifier"))
	}
	if set.ConnectionLimit < 1 || set.ConnectionLimit > 10000 {
		return jobs.Permanent(fmt.Errorf("invalid connection limit %d", set.ConnectionLimit))
	}
	// Never more than the instance holds, whatever the settings say (they
	// are lowered with a resize; this covers a move to a smaller size).
	if most, ok, err := s.dedicatedLimitMax(ctx, store.New(s.db), p); err != nil {
		return err
	} else if ok && set.ConnectionLimit > most {
		set.ConnectionLimit = most
	}
	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, p.OwnerRole).Scan(&exists); err != nil {
		return err
	}
	// Attributes are spelled out (spec §7.1), never inherited from defaults.
	login, err := s.loginAttr(ctx, p)
	if err != nil {
		return err
	}
	attrs := fmt.Sprintf("%s NOSUPERUSER NOCREATEDB %s NOREPLICATION NOBYPASSRLS INHERIT CONNECTION LIMIT %d PASSWORD %s",
		login, OwnerCreateRole(p), set.ConnectionLimit, literal(p.ScramVerifier))
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	if _, err := conn.Exec(ctx, verb+" ROLE "+ident(p.OwnerRole)+" "+attrs); err != nil {
		return fmt.Errorf("%s role: %w", strings.ToLower(verb), err)
	}

	if _, err := conn.Exec(ctx, tempFileLimit(p.OwnerRole, p.Tier)); err != nil {
		return fmt.Errorf("set temp_file_limit: %w", err)
	}
	for param, val := range map[string]string{
		"statement_timeout":                   set.StatementTimeout,
		"idle_in_transaction_session_timeout": set.IdleInTransactionTimeout,
	} {
		stmt := "ALTER ROLE " + ident(p.OwnerRole) + " RESET " + param
		if val != "" {
			if !validDuration(val) {
				return jobs.Permanent(fmt.Errorf("invalid %s %q", param, val))
			}
			stmt = "ALTER ROLE " + ident(p.OwnerRole) + " SET " + param + " = " + literal(val)
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("set %s: %w", param, err)
		}
	}
	return log.Info(ctx, "role", "role %s ready (connection limit %d)", p.OwnerRole, set.ConnectionLimit)
}

func (s *Service) ensureDatabase(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, p.DbName).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		// template0: nothing an operator added to template1 leaks in.
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident(p.DbName)+" OWNER "+ident(p.OwnerRole)+
			" TEMPLATE template0 ENCODING 'UTF8'"); err != nil {
			return fmt.Errorf("create database: %w", err)
		}
	}
	for _, stmt := range []string{
		"REVOKE ALL ON DATABASE " + ident(p.DbName) + " FROM PUBLIC",
		"GRANT CONNECT, TEMPORARY ON DATABASE " + ident(p.DbName) + " TO " + ident(p.OwnerRole),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("database grants: %w", err)
		}
	}
	return log.Info(ctx, "database", "database %s ready; CONNECT revoked from PUBLIC", p.DbName)
}

func (s *Service) hardenDatabase(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	conn, err := s.connectInstance(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	for _, stmt := range []string{
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		"ALTER SCHEMA public OWNER TO " + ident(p.OwnerRole),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("harden database: %w", err)
		}
	}
	if p.Tier == TierShared {
		if err := RestrictActivity(ctx, conn); err != nil {
			return err
		}
	}
	return log.Info(ctx, "schema", "schema public owned by %s; CREATE revoked from PUBLIC", p.OwnerRole)
}

func (s *Service) syncPooler(ctx context.Context, log *jobs.StepLogger, step, msg string) error {
	if err := s.pooler.Sync(ctx); err != nil {
		return fmt.Errorf("pooler sync: %w", err)
	}
	return log.Info(ctx, step, "%s; poolers reloaded", msg)
}

// smokeTest connects through both poolers as the project role, which
// proves the route and SCRAM passthrough end to end (spec §6.1 step 5).
// A project in a region with its own pooler hosts is tested through one
// of them.
func (s *Service) smokeTest(ctx context.Context, p store.Project, password string, log *jobs.StepLogger) error {
	if len(p.AllowedCidrs) > 0 {
		// The poolers take the owner only from the project's allowlist, and
		// xenibase-server isn't on it.
		return log.Info(ctx, "smoke", "skipped: the poolers take %s only from the project's IP allowlist", p.OwnerRole)
	}
	session, pooled := s.cfg.SmokeSessionAddr, s.cfg.SmokePooledAddr
	if s.pooler != nil {
		if rs, rp, ok := s.pooler.SmokeAddrs(p.Region); ok {
			session, pooled = rs, rp
		}
	}
	for _, target := range []struct{ mode, addr string }{
		{"session", session},
		{"transaction", pooled},
	} {
		if err := s.connectAsProject(ctx, target.addr, p, password); err != nil {
			return fmt.Errorf("smoke test via %s pooler (%s): %w", target.mode, target.addr, err)
		}
		if err := log.Info(ctx, "smoke", "connected through the %s pooler as %s", target.mode, p.OwnerRole); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) connectAsProject(ctx context.Context, addr string, p store.Project, password string) error {
	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://%s@%s/%s?sslmode=%s", p.OwnerRole, addr, p.DbName, s.cfg.SmokeSSLMode))
	if err != nil {
		return err
	}
	cfg.Password = password
	cfg.ConnectTimeout = 10 * time.Second
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var user, db string
	if err := conn.QueryRow(ctx, "SELECT current_user, current_database()").Scan(&user, &db); err != nil {
		return err
	}
	if user != p.OwnerRole || db != p.DbName {
		return fmt.Errorf("connected as %s to %s, want %s to %s", user, db, p.OwnerRole, p.DbName)
	}
	return nil
}

// rollbackCreate undoes a failed create: route, connections, database, and
// role, so a failed create leaves nothing behind (spec §6.1).
func (s *Service) rollbackCreate(ctx context.Context, op store.Operation, log *jobs.StepLogger, _ error) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	if p.Status == StatusActive {
		return nil // nothing to undo
	}
	if err := store.New(s.db).SoftDeleteProject(ctx, store.SoftDeleteProjectParams{ID: p.ID, Status: StatusError}); err != nil {
		return err
	}
	err = s.teardown(ctx, p, log)
	if err == nil {
		return nil
	}
	if p.Tier == TierDedicated {
		// The instance record outlives the project; the cleanup loop removes
		// it once the node answers.
		_ = log.Warn(ctx, "rollback", "the instance on its node will be removed automatically once the node is reachable")
		return err
	}
	// OnFail runs once, and the project is already gone from every list:
	// hand the database and roles to an operation that keeps retrying.
	cleanup, qerr := jobs.Enqueue(ctx, s.db, jobs.EnqueueParams{Kind: KindDropProjectObjects, ProjectID: &p.ID, Background: true,
		Params: map[string]any{"after": op.ID}})
	if qerr != nil {
		return fmt.Errorf("%w (and queueing the cleanup failed: %v)", err, qerr)
	}
	_ = log.Warn(ctx, "rollback", "operation %s keeps retrying the drop of %s and its roles", cleanup.ID, p.DbName)
	return err
}

// KindDropProjectObjects drops what a failed create's rollback could not:
// the database and roles of a project that is already deleted.
const KindDropProjectObjects = "drop_project_objects"

// runDropProjectObjects tears down a deleted shared project's database and
// roles. It refuses a live project.
func (s *Service) runDropProjectObjects(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	if p.DeletedAt == nil {
		return jobs.Permanent(fmt.Errorf("project %s is not deleted; nothing to drop", p.DbName))
	}
	if p.Tier != TierShared {
		return jobs.Permanent(errors.New("only a shared project's objects are dropped here (the reaper removes dedicated instances)"))
	}
	return s.teardown(ctx, p, log)
}

// teardown removes a project's pooler route and drops its database and role.
// The project must already be in a status the pooler does not route.
func (s *Service) teardown(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	// Drop pooled client connections first, then the route itself.
	if err := s.pooler.Kill(ctx, store.PoolerNames(p)...); err != nil {
		_ = log.Warn(ctx, "pooler", "KILL %s: %v (continuing)", p.DbName, err)
	}
	if err := s.syncPooler(ctx, log, "pooler", "route and auth entry removed"); err != nil {
		// A pooler that is down reads the new files when it starts; the
		// database and role must go regardless (a failed create cleans up).
		var re *pooler.ReloadError
		if !errors.As(err, &re) {
			return err
		}
		_ = log.Warn(ctx, "pooler", "route removed from the config files, but %v; continuing", err)
	}
	if p.Tier == TierDedicated {
		if s.Instances == nil {
			return jobs.Permanent(ErrNoDedicated)
		}
		return s.Instances.Destroy(ctx, p, log)
	}

	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	var terminated int
	if err := conn.QueryRow(ctx,
		`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
		p.DbName).Scan(&terminated); err != nil {
		return fmt.Errorf("terminate connections: %w", err)
	}
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident(p.DbName)+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database: %w", err)
	}
	if err := DropConsoleRole(ctx, conn, ConsoleRole(p.DbName), p.DbName); err != nil {
		return fmt.Errorf("drop console role: %w", err)
	}
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+ident(RestoreLogin(p.DbName))); err != nil {
		return fmt.Errorf("drop restore login: %w", err)
	}
	if err := s.dropMemberRoles(ctx, p); err != nil {
		return fmt.Errorf("drop member logins: %w", err)
	}
	// Backend services' roles (V4 §2.3); they own nothing outside the
	// dropped database.
	for _, r := range store.ServiceRoles(p.DbName) {
		if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+ident(r)); err != nil {
			return fmt.Errorf("drop role %s: %w", r, err)
		}
	}
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+ident(p.OwnerRole)); err != nil {
		return fmt.Errorf("drop role: %w", err)
	}
	return log.Info(ctx, "drop", "terminated %d connection(s); dropped database %s and role %s", terminated, p.DbName, p.OwnerRole)
}

// runDelete implements spec §6.2 steps 2-5.
func (s *Service) runDelete(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	if p.Status == StatusDeleted {
		return log.Info(ctx, "done", "project already deleted")
	}
	if p.Status != StatusDeleting {
		return jobs.Permanent(fmt.Errorf("project is %s, not deleting", p.Status))
	}
	if skip, _ := deleteParams(op); skip || s.FinalBackup == nil {
		if err := log.Warn(ctx, "backup", "final backup skipped"); err != nil {
			return err
		}
	} else if gone, err := s.databaseGone(ctx, p); err != nil {
		return err
	} else if gone {
		// An earlier attempt took the backup and tore the project down.
		if err := log.Info(ctx, "backup", "final backup not needed: an earlier attempt already dropped the database"); err != nil {
			return err
		}
	} else if err := s.FinalBackup(ctx, p, log); err != nil {
		return fmt.Errorf("final backup: %w", err)
	}
	if err := s.teardown(ctx, p, log); err != nil {
		return err
	}
	// Webhooks and jobs go with the project (V2 §9.3).
	if err := s.DropAutomation(ctx, p); err != nil {
		return err
	}
	if err := store.New(s.db).SoftDeleteProject(ctx, store.SoftDeleteProjectParams{ID: p.ID, Status: StatusDeleted}); err != nil {
		return err
	}
	return log.Info(ctx, "done", "project %s deleted", p.DbName)
}

// databaseGone reports whether p's database no longer exists: a dedicated
// project's instance is deleted, or a shared project's database is not on
// its cluster.
func (s *Service) databaseGone(ctx context.Context, p store.Project) (bool, error) {
	if p.Tier == TierDedicated {
		inst, err := store.New(s.db).GetInstance(ctx, p.InstanceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		return err == nil && inst.Status == "deleted", err
	}
	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())
	var exists bool
	err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, p.DbName).Scan(&exists)
	return !exists, err
}

// failDelete leaves a project whose delete gave up in "error", unrouted,
// so the operator can inspect it and retry the delete.
func (s *Service) failDelete(ctx context.Context, op store.Operation, log *jobs.StepLogger, _ error) error {
	p, err := s.project(ctx, op)
	if err != nil || p.Status != StatusDeleting {
		return err
	}
	if err := store.New(s.db).SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: StatusError}); err != nil {
		return err
	}
	return log.Warn(ctx, "rollback", "project marked error; retry the delete once the cause is fixed")
}

// runRotate sets the new verifier on the backend and the pooler, then
// proves the new password works.
func (s *Service) runRotate(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	if p.Status != StatusActive {
		return jobs.Permanent(fmt.Errorf("project is %s, not active", p.Status))
	}
	var params rotateParams
	if err := json.Unmarshal(op.Params, &params); err != nil {
		return jobs.Permanent(fmt.Errorf("invalid params: %w", err))
	}
	password, err := s.openPassword(p.ID, KindRotate, params.Secrets.opSecrets)
	if err != nil {
		return jobs.Permanent(err)
	}
	if err := s.setVerifier(ctx, p, params.Secrets.Verifier); err != nil {
		return err
	}
	if err := log.Info(ctx, "role", "new SCRAM verifier set on %s", p.OwnerRole); err != nil {
		return err
	}
	if err := s.syncPooler(ctx, log, "pooler", "auth entry updated"); err != nil {
		return err
	}
	if err := s.smokeTest(ctx, p, password, log); err != nil {
		return err
	}
	return log.Info(ctx, "done", "password rotated")
}

// rollbackRotate restores the previous verifier, so the old password keeps
// working if the new one could not be proven.
func (s *Service) rollbackRotate(ctx context.Context, op store.Operation, log *jobs.StepLogger, _ error) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	var params rotateParams
	if err := json.Unmarshal(op.Params, &params); err != nil || params.Secrets.PreviousVerifier == "" {
		return errors.New("no previous verifier to restore")
	}
	if err := s.setVerifier(ctx, p, params.Secrets.PreviousVerifier); err != nil {
		return err
	}
	return s.syncPooler(ctx, log, "rollback", "previous password restored")
}

func (s *Service) setVerifier(ctx context.Context, p store.Project, verifier string) error {
	if !crypto.IsSCRAMVerifier(verifier) || strings.Contains(verifier, "'") {
		return jobs.Permanent(errors.New("invalid SCRAM verifier"))
	}
	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "ALTER ROLE "+ident(p.OwnerRole)+" PASSWORD "+literal(verifier)); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return store.New(s.db).SetProjectVerifier(ctx, store.SetProjectVerifierParams{ID: p.ID, ScramVerifier: verifier})
}

// deleteParams reads a delete operation's options.
func deleteParams(op store.Operation) (skipFinalBackup bool, err error) {
	var p struct {
		SkipFinalBackup bool `json:"skip_final_backup"`
	}
	err = json.Unmarshal(op.Params, &p)
	return p.SkipFinalBackup, err
}
