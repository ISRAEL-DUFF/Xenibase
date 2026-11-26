package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
)

// Personal database credentials (V2 §3.5): every member gets their own
// login per project, so removing a member never means changing the app's
// password.
//
//   - Read/write logins (admins, developers) are members of the owner role
//     and switch to it as they connect (ALTER ROLE … SET role), so what they
//     create belongs to the project, not to them.
//   - Read-only logins are members of <db>_ro, which Xenibase keeps granted
//     SELECT on every schema the owner has, and start every transaction
//     read-only.
//
// Both carry the project's guardrails and connection limit.

// Database access levels.
const (
	AccessReadWrite = "read_write"
	AccessReadOnly  = "read_only"
)

// KindDropDBUser retries dropping a personal login that could not be
// dropped when its member was removed (the instance was unreachable).
const KindDropDBUser = "drop_db_user"

// ReadOnlyRole names a project's read-only group role.
func ReadOnlyRole(dbName string) string { return dbName + "_ro" }

// personalRoleName returns a new opaque login name for a project member.
func personalRoleName(dbName string) (string, error) {
	suffix, err := randomSuffix(6)
	if err != nil {
		return "", err
	}
	return dbName + "_u_" + suffix, nil
}

// Credentials is a member's login, with the password when it was just set.
type Credentials struct {
	Role     string
	Password string
	Access   string
}

// IssueCredentials creates or rotates userID's personal login on p with the
// given access, and returns the new password (shown once).
func (s *Service) IssueCredentials(ctx context.Context, p store.Project, userID uuid.UUID, access string) (Credentials, error) {
	if access != AccessReadWrite && access != AccessReadOnly {
		return Credentials{}, fmt.Errorf("invalid access %q", access)
	}
	if p.Status != StatusActive {
		return Credentials{}, fmt.Errorf("%w: the project is %s", ErrConflict, p.Status)
	}
	q := store.New(s.db)
	role, isNew := "", false
	cur, err := q.GetProjectDBUser(ctx, store.GetProjectDBUserParams{ProjectID: p.ID, UserID: userID, OrgID: p.OrgID})
	switch {
	case err == nil:
		role = cur.RoleName
	case errors.Is(err, pgx.ErrNoRows):
		if role, err = personalRoleName(p.DbName); err != nil {
			return Credentials{}, err
		}
		isNew = true
	default:
		return Credentials{}, err
	}
	password, err := GeneratePassword()
	if err != nil {
		return Credentials{}, err
	}
	verifier, err := crypto.SCRAMVerifier(password)
	if err != nil {
		return Credentials{}, err
	}
	u := store.ProjectDbUser{ProjectID: p.ID, UserID: userID, OrgID: p.OrgID, RoleName: role, ScramVerifier: verifier, Access: access}
	if err := s.applyDBUser(ctx, p, u); err != nil {
		return Credentials{}, err
	}
	if _, err := q.UpsertProjectDBUser(ctx, store.UpsertProjectDBUserParams{
		ProjectID: p.ID, UserID: userID, OrgID: p.OrgID, RoleName: role, ScramVerifier: verifier, Access: access,
	}); err != nil {
		if isNew {
			// Nothing would know the login: take it off the instance again
			// (teardown also drops any <db>_u_* login it finds).
			if derr := s.dropDBUser(context.WithoutCancel(ctx), p.InstanceID, p.DbName, p.OwnerRole, role); derr != nil {
				s.log.Warn("drop a login whose metadata could not be written", "role", role, "err", derr)
			}
		}
		return Credentials{}, err
	}
	if err := s.pooler.Sync(ctx); err != nil {
		return Credentials{}, fmt.Errorf("pooler sync: %w", err)
	}
	return Credentials{Role: role, Password: password, Access: access}, nil
}

// SetCredentialAccess changes an existing login's access after a role
// change (a read-only member made developer, or back). It is a no-op for
// members without a login.
func (s *Service) SetCredentialAccess(ctx context.Context, p store.Project, userID uuid.UUID, access string) error {
	q := store.New(s.db)
	cur, err := q.GetProjectDBUser(ctx, store.GetProjectDBUserParams{ProjectID: p.ID, UserID: userID, OrgID: p.OrgID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && cur.Access == access) {
		return nil
	}
	if err != nil {
		return err
	}
	cur.Access = access
	if err := s.applyDBUser(ctx, p, cur); err != nil {
		return err
	}
	return q.SetProjectDBUserAccess(ctx, store.SetProjectDBUserAccessParams{ProjectID: p.ID, UserID: userID, OrgID: p.OrgID, Access: access})
}

// RevokeCredentials removes userID's login from p at once: the poolers
// stop accepting it, its connections end, and the role is dropped (or,
// if the instance cannot be reached, a drop_db_user operation retries).
func (s *Service) RevokeCredentials(ctx context.Context, p store.Project, userID uuid.UUID) error {
	q := store.New(s.db)
	cur, err := q.GetProjectDBUser(ctx, store.GetProjectDBUserParams{ProjectID: p.ID, UserID: userID, OrgID: p.OrgID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := q.DeleteProjectDBUser(ctx, store.DeleteProjectDBUserParams{ProjectID: p.ID, UserID: userID, OrgID: p.OrgID}); err != nil {
		return err
	}
	// The auth file loses the login first, so no new connection gets in.
	syncErr := s.pooler.Sync(ctx)
	if syncErr != nil {
		s.log.Warn("pooler sync after revoking a login", "role", cur.RoleName, "err", syncErr)
	}
	if err := s.pooler.KillUser(ctx, cur.RoleName); err != nil {
		s.log.Warn("drop a revoked login's pooler clients", "role", cur.RoleName, "err", err)
	}
	if err := s.dropDBUser(ctx, p.InstanceID, p.DbName, p.OwnerRole, cur.RoleName); err != nil {
		s.log.Warn("drop a revoked login; retrying as an operation", "role", cur.RoleName, "err", err)
		pid := p.ID
		if _, qerr := jobs.Enqueue(ctx, s.db, jobs.EnqueueParams{Kind: KindDropDBUser, ProjectID: &pid, Background: true,
			Params: dropDBUserParams{Role: cur.RoleName}}); qerr != nil {
			return errors.Join(err, qerr)
		}
	}
	return nil
}

type dropDBUserParams struct {
	Role string `json:"role"`
}

func (s *Service) runDropDBUser(ctx context.Context, op store.Operation, log *jobs.StepLogger) error {
	p, err := s.project(ctx, op)
	if err != nil {
		return err
	}
	var params dropDBUserParams
	if err := json.Unmarshal(op.Params, &params); err != nil || !strings.HasPrefix(params.Role, p.DbName+"_u_") {
		return jobs.Permanent(fmt.Errorf("invalid drop_db_user parameters"))
	}
	if err := s.dropDBUser(ctx, p.InstanceID, p.DbName, p.OwnerRole, params.Role); err != nil {
		return err
	}
	return log.Info(ctx, "role", "login %s dropped", params.Role)
}

// SyncMemberRoles makes p's instance match the metadata: the read-only
// group role and its grants, and every personal login with its verifier,
// membership, and guardrails. Tier moves, restores, and imports run it,
// so members' credentials survive them (V2 §3.5).
func (s *Service) SyncMemberRoles(ctx context.Context, p store.Project, log *jobs.StepLogger) error {
	users, err := store.New(s.db).ListProjectDBUsers(ctx, p.ID)
	if err != nil {
		return err
	}
	if err := s.ensureReadOnlyRole(ctx, p); err != nil {
		return err
	}
	for _, u := range users {
		if err := s.applyDBUser(ctx, p, u); err != nil {
			return fmt.Errorf("member login %s: %w", u.RoleName, err)
		}
	}
	if log != nil && len(users) > 0 {
		return log.Info(ctx, "members", "%d member login(s) and the read-only role are in place", len(users))
	}
	return nil
}

// EnsureReadOnlyRole creates or repairs p's read-only group role.
func (s *Service) EnsureReadOnlyRole(ctx context.Context, p store.Project) error {
	return s.ensureReadOnlyRole(ctx, p)
}

// ensureReadOnlyRole creates <db>_ro and grants it read access to every
// schema in the project database, now and (through default privileges)
// for whatever the owner creates later.
func (s *Service) ensureReadOnlyRole(ctx context.Context, p store.Project) error {
	ro := ReadOnlyRole(p.DbName)
	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	var exists bool
	err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, ro).Scan(&exists)
	if err == nil && !exists {
		_, err = conn.Exec(ctx, "CREATE ROLE "+ident(ro)+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT")
	}
	if err == nil {
		_, err = conn.Exec(ctx, "GRANT CONNECT ON DATABASE "+ident(p.DbName)+" TO "+ident(ro))
	}
	conn.Close(context.Background())
	if err != nil {
		return fmt.Errorf("read-only role: %w", err)
	}

	db, err := s.connectInstance(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer db.Close(context.Background())
	rows, err := db.Query(ctx, `SELECT nspname FROM pg_namespace
		WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema' ORDER BY nspname`)
	if err != nil {
		return err
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	stmts := []string{
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + ident(p.OwnerRole) + " GRANT USAGE ON SCHEMAS TO " + ident(ro),
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + ident(p.OwnerRole) + " GRANT SELECT ON TABLES TO " + ident(ro),
	}
	for _, sch := range schemas {
		stmts = append(stmts,
			"GRANT USAGE ON SCHEMA "+ident(sch)+" TO "+ident(ro),
			"GRANT SELECT ON ALL TABLES IN SCHEMA "+ident(sch)+" TO "+ident(ro))
	}
	for _, st := range stmts {
		if _, err := db.Exec(ctx, st); err != nil {
			return fmt.Errorf("read-only grants: %w", err)
		}
	}
	return nil
}

// applyDBUser creates or updates one personal login on p's instance.
func (s *Service) applyDBUser(ctx context.Context, p store.Project, u store.ProjectDbUser) error {
	if !crypto.IsSCRAMVerifier(u.ScramVerifier) || strings.Contains(u.ScramVerifier, "'") {
		return jobs.Permanent(errors.New("invalid SCRAM verifier"))
	}
	if !strings.HasPrefix(u.RoleName, p.DbName+"_u_") {
		return jobs.Permanent(fmt.Errorf("login %s does not belong to %s", u.RoleName, p.DbName))
	}
	set, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		return err
	}
	if u.Access == AccessReadOnly {
		if err := s.ensureReadOnlyRole(ctx, p); err != nil {
			return err
		}
	}
	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	role, ro := ident(u.RoleName), ident(ReadOnlyRole(p.DbName))
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, u.RoleName).Scan(&exists); err != nil {
		return err
	}
	login, err := s.loginAttr(ctx, p)
	if err != nil {
		return err
	}
	attrs := fmt.Sprintf("%s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT CONNECTION LIMIT %d PASSWORD %s",
		login, set.ConnectionLimit, literal(u.ScramVerifier))
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	stmts := []string{verb + " ROLE " + role + " " + attrs}
	if u.Access == AccessReadWrite {
		stmts = append(stmts,
			"REVOKE "+ro+" FROM "+role,
			"GRANT "+ident(p.OwnerRole)+" TO "+role+" WITH INHERIT TRUE, SET TRUE",
			"ALTER ROLE "+role+" SET role = "+literal(p.OwnerRole),
			"ALTER ROLE "+role+" RESET default_transaction_read_only")
	} else {
		stmts = append(stmts,
			"REVOKE "+ident(p.OwnerRole)+" FROM "+role,
			"GRANT "+ro+" TO "+role+" WITH INHERIT TRUE, SET FALSE",
			"ALTER ROLE "+role+" RESET role",
			"ALTER ROLE "+role+" SET default_transaction_read_only = on")
	}
	stmts = append(stmts, tempFileLimit(u.RoleName, p.Tier))
	for param, val := range map[string]string{
		"statement_timeout":                   set.StatementTimeout,
		"idle_in_transaction_session_timeout": set.IdleInTransactionTimeout,
	} {
		st := "ALTER ROLE " + role + " RESET " + param
		if val != "" {
			if !validDuration(val) {
				return jobs.Permanent(fmt.Errorf("invalid %s %q", param, val))
			}
			st = "ALTER ROLE " + role + " SET " + param + " = " + literal(val)
		}
		stmts = append(stmts, st)
	}
	for _, st := range stmts {
		if _, err := conn.Exec(ctx, st); err != nil {
			// REVOKE of a membership that does not exist only warns; a
			// missing read-only role on a read/write login is fine too.
			var pgErr interface{ SQLState() string }
			if strings.HasPrefix(st, "REVOKE ") && errors.As(err, &pgErr) && pgErr.SQLState() == "42704" {
				continue
			}
			return fmt.Errorf("member login %s: %w", u.RoleName, err)
		}
	}
	if exists {
		// Sessions keep the settings and role they logged in with, and the
		// poolers reuse server connections: end them, so the change holds
		// from the next query on.
		if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1`, u.RoleName); err != nil {
			return fmt.Errorf("member login %s: end sessions: %w", u.RoleName, err)
		}
	}
	return nil
}

// dropDBUser ends a login's sessions, hands anything it owns to the
// project owner, and drops it. A login that is already gone is fine.
func (s *Service) dropDBUser(ctx context.Context, instanceID uuid.UUID, dbName, ownerRole, role string) error {
	conn, err := s.connectInstance(ctx, instanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var exists, dbExists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
		EXISTS (SELECT 1 FROM pg_database WHERE datname = $2)`, role, dbName).Scan(&exists, &dbExists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1`, role); err != nil {
		return err
	}
	if dbExists {
		db, err := s.connectInstance(ctx, instanceID, dbName)
		if err != nil {
			return err
		}
		for _, st := range []string{
			"REASSIGN OWNED BY " + ident(role) + " TO " + ident(ownerRole),
			"DROP OWNED BY " + ident(role),
		} {
			if _, err := db.Exec(ctx, st); err != nil {
				db.Close(context.Background())
				return fmt.Errorf("drop login %s: %w", role, err)
			}
		}
		db.Close(context.Background())
	}
	if _, err := conn.Exec(ctx, "DROP ROLE IF EXISTS "+ident(role)); err != nil {
		return fmt.Errorf("drop login %s: %w", role, err)
	}
	return nil
}

// dropMemberRoles removes every personal login and the read-only role of a
// project that is being torn down.
func (s *Service) dropMemberRoles(ctx context.Context, p store.Project) error {
	users, err := store.New(s.db).ListProjectDBUsers(ctx, p.ID)
	if err != nil {
		return err
	}
	roles := make([]string, 0, len(users))
	for _, u := range users {
		roles = append(roles, u.RoleName)
	}
	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	// And any personal login the metadata lost (a revoke whose retry gave
	// up, an issue whose metadata write failed).
	rows, err := conn.Query(ctx, `SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1)`, p.DbName+"_u_")
	if err != nil {
		return err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, r := range found {
		if !slices.Contains(roles, r) {
			roles = append(roles, r)
		}
	}
	for _, r := range roles {
		if err := s.dropDBUser(ctx, p.InstanceID, p.DbName, p.OwnerRole, r); err != nil {
			return err
		}
	}
	ro := ReadOnlyRole(p.DbName)
	var exists, dbExists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
		EXISTS (SELECT 1 FROM pg_database WHERE datname = $2)`, ro, p.DbName).Scan(&exists, &dbExists); err != nil || !exists {
		return err
	}
	if dbExists {
		db, err := s.connectInstance(ctx, p.InstanceID, p.DbName)
		if err != nil {
			return err
		}
		_, err = db.Exec(ctx, "DROP OWNED BY "+ident(ro))
		db.Close(context.Background())
		if err != nil {
			return fmt.Errorf("drop read-only role: %w", err)
		}
	}
	_, err = conn.Exec(ctx, "DROP ROLE IF EXISTS "+ident(ro))
	return err
}
