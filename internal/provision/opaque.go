package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
)

// Operation kinds for opaque naming (V2 §10.2).
const (
	// KindRenameOpaque renames a V1 project's database to an opaque name and
	// keeps the old name as a pooler alias, so connection strings still work.
	KindRenameOpaque = "rename_opaque"
	// KindSwitchCredentials gives a renamed V1 project an opaque owner role
	// with a new password; the old role keeps working for a grace period.
	KindSwitchCredentials = "switch_credentials"
	// KindExpireLegacy drops the old role and alias once the grace period
	// ends.
	KindExpireLegacy = "expire_legacy_credentials"
)

// DefaultCredentialGrace is how long V1 credentials keep working after a
// switch to opaque ones (V2 §10.2).
const DefaultCredentialGrace = 7 * 24 * time.Hour

// freezeWait bounds how long a rename waits for in-flight transactions.
const renameFreezeWait = 10 * time.Second

type renameParams struct {
	OldName string `json:"old_name"`
	NewName string `json:"new_name"`
}

func (s *Service) opaqueKinds() map[string]jobs.Kind {
	return map[string]jobs.Kind{
		KindRenameOpaque:      {Handler: s.runRenameOpaque, OnFail: s.rollbackRenameOpaque, MaxAttempts: 3, Class: jobs.ClassScheduled},
		KindSwitchCredentials: {Handler: s.runSwitchCredentials, OnFail: s.rollbackSwitchCredentials, MaxAttempts: 3},
		KindExpireLegacy:      {Handler: s.runExpireLegacy, MaxAttempts: 10, Class: jobs.ClassScheduled},
		KindReclaimSpace:      {Handler: s.runReclaimSpace, MaxAttempts: 2},
	}
}

// newOpaqueName returns an opaque name no project uses.
func (s *Service) newOpaqueName(ctx context.Context) (string, error) {
	for range 10 {
		name, err := OpaqueDBName()
		if err != nil {
			return "", err
		}
		taken, err := store.New(s.db).DBNameTaken(ctx, name)
		if err != nil {
			return "", err
		}
		if !taken {
			return name, nil
		}
	}
	return "", errors.New("could not find a free opaque name")
}

// ScheduleOpaqueRenames queues rename_opaque for every active project whose
// database still has a V1 name, and expires switched credentials whose
// grace period is over. A busy project is picked up on the next sweep.
func (s *Service) ScheduleOpaqueRenames(ctx context.Context, now time.Time) (int, error) {
	q := store.New(s.db)
	todo, err := q.ProjectsToRename(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range todo {
		name, err := s.newOpaqueName(ctx)
		if err != nil {
			return n, err
		}
		_, err = s.EnqueueExclusive(ctx, p.ID, []string{StatusActive}, "", KindRenameOpaque,
			renameParams{OldName: p.DbName, NewName: name}, nil, func(cur store.Project) error {
				if IsOpaque(cur.DbName) {
					return fmt.Errorf("%w: already opaque", ErrConflict)
				}
				return nil
			})
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return n, err
		}
		n++
	}
	expired, err := q.ProjectsLegacyExpired(ctx, now)
	if err != nil {
		return n, err
	}
	for _, p := range expired {
		_, err := s.EnqueueExclusive(ctx, p.ID, []string{StatusActive, StatusError}, "", KindExpireLegacy, map[string]any{}, nil, nil)
		if err != nil && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
			return n, err
		}
	}
	return n, nil
}

// runRenameOpaque renames the backend database with the project's pooler
// route paused, renames the roles named after it, and routes the old name
// to the new database.
func (s *Service) runRenameOpaque(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	var params renameParams
	if err := json.Unmarshal(op.Params, &params); err != nil || !IsOpaque(params.NewName) || params.OldName == "" {
		return jobs.Permanent(fmt.Errorf("invalid params: %s", op.Params))
	}
	old, name := params.OldName, params.NewName
	if p.DbName == name {
		return log.Info(ctx, "done", "already renamed")
	}
	if p.DbName != old {
		return jobs.Permanent(fmt.Errorf("project database is %s, not %s", p.DbName, old))
	}

	// Hold the project's clients while the database has neither name.
	if _, err := s.pooler.Freeze(ctx, renameFreezeWait, old); err != nil {
		_ = log.Warn(ctx, "pause", "pausing %s on the poolers: %v (continuing)", old, err)
	}
	defer func() {
		_ = s.pooler.Resume(context.WithoutCancel(ctx), old, name)
	}()

	if err := s.renameObjects(ctx, p, old, name); err != nil {
		return err
	}
	if p.Tier == TierShared {
		conn, err := s.connectInstance(ctx, p.InstanceID, name)
		if err != nil {
			return err
		}
		err = RestrictActivity(ctx, conn)
		conn.Close(context.Background())
		if err != nil {
			return err
		}
	}
	if err := log.Info(ctx, "rename", "database and its roles renamed to the opaque name"); err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.SetProjectBackendName(ctx, store.SetProjectBackendNameParams{ID: p.ID, DbName: name, AliasDbName: &old}); err != nil {
			return err
		}
		users, err := q.ListProjectDBUsers(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, u := range users {
			if renamed, ok := strings.CutPrefix(u.RoleName, old+"_u_"); ok {
				if err := q.RenameProjectDBUser(ctx, store.RenameProjectDBUserParams{ProjectID: p.ID, OldName: u.RoleName, NewName: name + "_u_" + renamed}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.syncPooler(ctx, log, "pooler", "the old name now routes to the opaque database"); err != nil {
		return err
	}
	if err := s.pooler.Resume(ctx, old); err != nil && !strings.Contains(err.Error(), "not paused") {
		_ = log.Warn(ctx, "resume", "%v", err)
	}
	if err := s.checkAlias(ctx, p.InstanceID, old, name); err != nil {
		return err
	}
	return log.Info(ctx, "done", "the project's existing connection strings keep working")
}

// renameObjects renames database old to name and the roles derived from
// it (the read-only role and personal logins); the console role is
// dropped, and recreated on first use. Each step is skipped if done.
func (s *Service) renameObjects(ctx context.Context, p store.Project, old, name string) error {
	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var oldExists, newExists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1),
		EXISTS (SELECT 1 FROM pg_database WHERE datname = $2)`, old, name).Scan(&oldExists, &newExists); err != nil {
		return err
	}
	switch {
	case oldExists && newExists:
		return jobs.Permanent(fmt.Errorf("both %s and %s exist", old, name))
	case oldExists:
		if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, old); err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, "ALTER DATABASE "+ident(old)+" RENAME TO "+ident(name)); err != nil {
			return fmt.Errorf("rename database: %w", err)
		}
	case !newExists:
		return jobs.Permanent(fmt.Errorf("database %s does not exist", old))
	}
	rows, err := conn.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname = $1 OR starts_with(rolname, $2)`,
		ReadOnlyRole(old), old+"_u_")
	if err != nil {
		return err
	}
	roles, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, r := range roles {
		to := name + strings.TrimPrefix(r, old)
		if _, err := conn.Exec(ctx, "ALTER ROLE "+ident(r)+" RENAME TO "+ident(to)); err != nil {
			return fmt.Errorf("rename role: %w", err)
		}
		_ = s.pooler.KillUser(ctx, r)
	}
	return DropConsoleRole(ctx, conn, ConsoleRole(old), name)
}

// checkAlias is the rename's smoke test: every pooler routes the old name
// to the new database, which answers.
func (s *Service) checkAlias(ctx context.Context, instanceID uuid.UUID, alias, name string) error {
	for _, a := range s.pooler.Admins() {
		dbs, err := a.Databases(ctx)
		if err != nil {
			return err
		}
		if dbs[alias] != name || dbs[name] != name {
			return fmt.Errorf("pooler %s routes %s to %q and %s to %q", a.Name, alias, dbs[alias], name, dbs[name])
		}
	}
	conn, err := s.connectInstance(ctx, instanceID, name)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return conn.Ping(ctx)
}

// rollbackRenameOpaque puts the V1 name back.
func (s *Service) rollbackRenameOpaque(ctx context.Context, op store.Operation, log *jobs.StepLogger, _ error) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	var params renameParams
	if err := json.Unmarshal(op.Params, &params); err != nil || params.OldName == "" || params.NewName == "" {
		return nil
	}
	old, name := params.OldName, params.NewName
	defer func() { _ = s.pooler.Resume(context.WithoutCancel(ctx), old, name) }()
	if err := s.renameObjects(ctx, p, name, old); err != nil && !jobs.IsPermanent(err) {
		return err
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.SetProjectBackendName(ctx, store.SetProjectBackendNameParams{ID: p.ID, DbName: old}); err != nil {
			return err
		}
		users, err := q.ListProjectDBUsers(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, u := range users {
			if rest, ok := strings.CutPrefix(u.RoleName, name+"_u_"); ok {
				if err := q.RenameProjectDBUser(ctx, store.RenameProjectDBUserParams{ProjectID: p.ID, OldName: u.RoleName, NewName: old + "_u_" + rest}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.syncPooler(ctx, log, "rollback", "the project is back under its V1 name")
}

// Switched is returned once by SwitchCredentials: the new password.
type Switched struct {
	Project   store.Project
	Operation store.Operation
	Password  string
	Until     time.Time
}

type switchParams struct {
	OwnerRole string        `json:"owner_role"`
	Until     time.Time     `json:"until"`
	Secrets   switchSecrets `json:"secrets"`
}

// switchSecrets adds the new owner's SCRAM verifier to the handoff, so it
// is scrubbed with it (see rotateSecrets).
type switchSecrets struct {
	opSecrets
	Verifier string `json:"verifier"`
}

// ErrNotLegacy means a project has no V1 credentials to switch from.
var ErrNotLegacy = errors.New("the project already uses opaque credentials")

// CanSwitchCredentials reports whether p still uses a V1 owner role.
func CanSwitchCredentials(p store.Project) bool {
	return IsOpaque(p.DbName) && p.OwnerRole != OwnerRoleName(p.DbName) && p.LegacyUntil == nil
}

// SwitchCredentials queues the switch to opaque credentials (V2 §10.2): a
// new owner role and password; the V1 role keeps working for grace.
func (s *Service) SwitchCredentials(ctx context.Context, projectID uuid.UUID, grace time.Duration, now time.Time, by *uuid.UUID) (Switched, error) {
	if grace < time.Hour || grace > 90*24*time.Hour {
		return Switched{}, fmt.Errorf("%w: the grace period must be between 1 hour and 90 days", ErrInvalid)
	}
	password, err := GeneratePassword()
	if err != nil {
		return Switched{}, err
	}
	verifier, err := crypto.SCRAMVerifier(password)
	if err != nil {
		return Switched{}, err
	}
	sec, err := s.sealPassword(projectID, KindSwitchCredentials, password)
	if err != nil {
		return Switched{}, err
	}
	until := now.Add(grace)
	var out Switched
	err = s.withIdleProject(ctx, projectID, []string{StatusActive}, func(tx pgx.Tx, p store.Project) error {
		if !CanSwitchCredentials(p) {
			if !IsOpaque(p.DbName) {
				return fmt.Errorf("%w: the project is being renamed to an opaque database first; try again shortly", ErrConflict)
			}
			return ErrNotLegacy
		}
		op, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{
			Kind: KindSwitchCredentials, ProjectID: &p.ID, CreatedBy: by,
			Params: switchParams{OwnerRole: OwnerRoleName(p.DbName), Until: until, Secrets: switchSecrets{opSecrets: sec, Verifier: verifier}},
		})
		out = Switched{Project: p, Operation: op, Password: password, Until: until}
		return err
	})
	return out, err
}

// runSwitchCredentials creates the opaque owner, hands it everything the
// V1 owner owns, and makes the V1 role act as it until the grace ends.
func (s *Service) runSwitchCredentials(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	var params switchParams
	if err := json.Unmarshal(op.Params, &params); err != nil || params.OwnerRole == "" {
		return jobs.Permanent(fmt.Errorf("invalid params: %w", err))
	}
	password, err := s.openPassword(p.ID, KindSwitchCredentials, params.Secrets.opSecrets)
	if err != nil {
		return jobs.Permanent(err)
	}
	if p.OwnerRole == params.OwnerRole {
		return log.Info(ctx, "done", "already switched")
	}
	old := p.OwnerRole
	settings, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		return jobs.Permanent(err)
	}
	next := p
	next.OwnerRole, next.ScramVerifier = params.OwnerRole, params.Secrets.Verifier
	if err := s.ensureRole(ctx, next, settings, log); err != nil {
		return err
	}
	if err := s.handOver(ctx, p, old, next.OwnerRole); err != nil {
		return err
	}
	if err := log.Info(ctx, "owner", "%s owns the database and its objects; the old role acts as it", next.OwnerRole); err != nil {
		return err
	}
	if err := store.New(s.db).SwitchProjectCredentials(ctx, store.SwitchProjectCredentialsParams{
		ID: p.ID, OwnerRole: next.OwnerRole, ScramVerifier: next.ScramVerifier,
		LegacyOwnerRole: &old, LegacyScramVerifier: &p.ScramVerifier, LegacyUntil: &params.Until,
	}); err != nil {
		return err
	}
	next.LegacyOwnerRole, next.LegacyUntil = &old, &params.Until
	if err := s.SyncMemberRoles(ctx, next, log); err != nil {
		return err
	}
	if err := s.syncPooler(ctx, log, "pooler", "the new login is in the auth file; the old one stays until the grace period ends"); err != nil {
		return err
	}
	if err := s.smokeTest(ctx, next, password, log); err != nil {
		return err
	}
	return log.Info(ctx, "done", "switched to opaque credentials; %s works until %s", old, params.Until.UTC().Format(time.RFC3339))
}

// handOver transfers ownership from old to owner in p's database and
// makes old a member of owner that assumes it at login.
func (s *Service) handOver(ctx context.Context, p store.Project, old, owner string) error {
	conn, err := s.connectInstance(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	for _, stmt := range []string{
		"GRANT CONNECT, TEMPORARY ON DATABASE " + ident(p.DbName) + " TO " + ident(owner),
		"REASSIGN OWNED BY " + ident(old) + " TO " + ident(owner),
		"ALTER DATABASE " + ident(p.DbName) + " OWNER TO " + ident(owner),
		"GRANT " + ident(owner) + " TO " + ident(old) + " WITH INHERIT TRUE, SET TRUE",
		"ALTER ROLE " + ident(old) + " SET role = " + literal(owner),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", strings.SplitN(stmt, " ", 3)[0]+" "+strings.SplitN(stmt, " ", 3)[1], err)
		}
	}
	return nil
}

// rollbackSwitchCredentials hands everything back to the V1 role.
func (s *Service) rollbackSwitchCredentials(ctx context.Context, op store.Operation, log *jobs.StepLogger, _ error) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	var params switchParams
	if err := json.Unmarshal(op.Params, &params); err != nil || params.OwnerRole == "" {
		return nil
	}
	old := p.OwnerRole
	if p.LegacyOwnerRole != nil {
		old = *p.LegacyOwnerRole
	}
	if old == params.OwnerRole {
		return nil
	}
	conn, err := s.connectInstance(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, params.OwnerRole).Scan(&exists); err != nil {
		return err
	}
	if exists {
		for _, stmt := range []string{
			"ALTER ROLE " + ident(old) + " RESET role",
			"REASSIGN OWNED BY " + ident(params.OwnerRole) + " TO " + ident(old),
			"ALTER DATABASE " + ident(p.DbName) + " OWNER TO " + ident(old),
			"DROP OWNED BY " + ident(params.OwnerRole),
			"DROP ROLE " + ident(params.OwnerRole),
		} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("rollback: %w", err)
			}
		}
	}
	if p.LegacyOwnerRole != nil {
		if err := store.New(s.db).SwitchProjectCredentials(ctx, store.SwitchProjectCredentialsParams{
			ID: p.ID, OwnerRole: old, ScramVerifier: *p.LegacyScramVerifier,
		}); err != nil {
			return err
		}
		p.OwnerRole, p.ScramVerifier, p.LegacyOwnerRole, p.LegacyUntil = old, *p.LegacyScramVerifier, nil, nil
	}
	if err := s.SyncMemberRoles(ctx, p, log); err != nil {
		return err
	}
	return s.syncPooler(ctx, log, "rollback", "the V1 credentials are the project's again")
}

// runExpireLegacy ends the grace period: the V1 role's sessions end, the
// role is dropped, and the V1 alias stops routing.
func (s *Service) runExpireLegacy(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	if p.LegacyOwnerRole == nil {
		return log.Info(ctx, "done", "nothing to expire")
	}
	old := *p.LegacyOwnerRole
	if err := s.pooler.KillUser(ctx, old); err != nil {
		_ = log.Warn(ctx, "pooler", "KILL_CLIENT %s: %v (continuing)", old, err)
	}
	conn, err := s.connectInstance(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, old).Scan(&exists); err != nil {
		return err
	}
	if exists {
		for _, stmt := range []string{
			"ALTER ROLE " + ident(old) + " NOLOGIN",
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = " + literal(old),
			"REASSIGN OWNED BY " + ident(old) + " TO " + ident(p.OwnerRole),
			"DROP OWNED BY " + ident(old),
			"DROP ROLE " + ident(old),
		} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("drop %s: %w", old, err)
			}
		}
	}
	if err := store.New(s.db).ClearProjectLegacy(ctx, p.ID); err != nil {
		return err
	}
	if err := s.syncPooler(ctx, log, "pooler", "the V1 role and database alias are gone"); err != nil {
		return err
	}
	return log.Info(ctx, "done", "the grace period ended; only the opaque credentials work now")
}
