package provision

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/store"
)

// consoleConnLimit caps a project's concurrent console-login sessions.
const consoleConnLimit = 5

// ConsoleRole names the login the SQL console and table browser use for a
// project database (spec §8.5). It can SET ROLE to the owner but inherits
// nothing, so RESET ROLE leaves a session with no privileges rather than
// the superuser's.
func ConsoleRole(dbName string) string { return dbName + "_console" }

// ConsoleReadOnlyRole names the login read-only console sessions use (a
// read_only member, a read-scope token, the table browser's raw filter).
// It is granted only the read-only role, WITH INHERIT FALSE, SET TRUE, so
// a statement that switches role (set_config('role', ...)) has no owner to
// switch to. The owner's console login could, and RLS does not apply to
// the owner on its own tables.
func ConsoleReadOnlyRole(dbName string) string { return ConsoleRole(dbName) + "_ro" }

// ConsoleLogin is the login a console session for dbName connects as.
func ConsoleLogin(dbName string, readOnly bool) string {
	if readOnly {
		return ConsoleReadOnlyRole(dbName)
	}
	return ConsoleRole(dbName)
}

// DropConsoleRole removes a console role and its read-only twin
// (role+"_ro"), first revoking what they hold on database (when that
// database still exists).
func DropConsoleRole(ctx context.Context, conn *pgx.Conn, role, database string) error {
	for _, r := range []string{role + "_ro", role} {
		if err := dropConsoleLogin(ctx, conn, r, database); err != nil {
			return err
		}
	}
	return nil
}

func dropConsoleLogin(ctx context.Context, conn *pgx.Conn, role, database string) error {
	var exists, dbExists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1),
		EXISTS (SELECT 1 FROM pg_database WHERE datname = $2)`, role, database).Scan(&exists, &dbExists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if dbExists {
		if _, err := conn.Exec(ctx, "REVOKE ALL ON DATABASE "+ident(database)+" FROM "+ident(role)); err != nil {
			return err
		}
	}
	_, err := conn.Exec(ctx, "DROP ROLE "+ident(role))
	return err
}

// ConsolePassword is the console login's password, derived from the master
// key.
func (s *Service) ConsolePassword(role string) string {
	return base64.RawURLEncoding.EncodeToString(s.keyring.Derive("xenibase console role "+role, 32))
}

// EnsureConsoleRole creates or repairs the project's console logins (and
// its read-only role).
func (s *Service) EnsureConsoleRole(ctx context.Context, p store.Project) error {
	if err := s.EnsureReadOnlyRole(ctx, p); err != nil {
		return fmt.Errorf("read-only role: %w", err)
	}
	conn, err := s.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return s.ApplyConsoleRoles(ctx, conn, p)
}

// ApplyConsoleRoles creates or repairs p's two console logins over conn,
// a superuser connection to the instance: <db>_console, which may become
// the owner or the read-only role, and <db>_console_ro, which may become
// only the read-only role. The read-only role must exist.
func (s *Service) ApplyConsoleRoles(ctx context.Context, conn *pgx.Conn, p store.Project) error {
	ro := ReadOnlyRole(p.DbName)
	for _, l := range []struct {
		role  string
		roles []string
	}{
		{ConsoleRole(p.DbName), []string{p.OwnerRole, ro}},
		{ConsoleReadOnlyRole(p.DbName), []string{ro}},
	} {
		if err := s.applyConsoleLogin(ctx, conn, p, l.role, l.roles); err != nil {
			return err
		}
		s.log.Info("console role ready", "project_id", p.ID, "role", l.role)
	}
	return nil
}

func (s *Service) applyConsoleLogin(ctx context.Context, conn *pgx.Conn, p store.Project, role string, roles []string) error {
	verifier, err := crypto.SCRAMVerifier(s.ConsolePassword(role))
	if err != nil {
		return err
	}
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&exists); err != nil {
		return err
	}
	verb := "ALTER"
	if !exists {
		verb = "CREATE"
	}
	stmts := []string{
		fmt.Sprintf("%s ROLE %s LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT %d PASSWORD '%s'",
			verb, ident(role), consoleConnLimit, verifier),
	}
	for _, r := range roles {
		stmts = append(stmts, "GRANT "+ident(r)+" TO "+ident(role)+" WITH INHERIT FALSE, SET TRUE")
	}
	stmts = append(stmts,
		"GRANT CONNECT ON DATABASE "+ident(p.DbName)+" TO "+ident(role),
		// Top queries show the app's workload, not the console's.
		"ALTER ROLE "+ident(role)+" SET pg_stat_statements.track = 'none'",
	)
	if p.Tier == TierShared {
		stmts = append(stmts, "ALTER ROLE "+ident(role)+" SET temp_file_limit = '"+TempFileLimit+"'")
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			var pe *pgconn.PgError
			if verb == "CREATE" && errors.As(err, &pe) && pe.Code == "42710" {
				// Created concurrently; set the password anyway.
				stmt = "ALTER" + strings.TrimPrefix(stmt, "CREATE")
				if _, err = conn.Exec(ctx, stmt); err == nil {
					continue
				}
			}
			return err
		}
	}
	return nil
}

// RestoreConn is how pg_restore reaches a project database when the dump
// is the tenant's (an import, a branch, a backup): as the console login,
// assuming the owner with --role. Restoring a dump runs SQL of the source's
// choosing, and the console login holds nothing itself, so a RESET ROLE in
// it gains nothing (an admin login would be the superuser).
//
// database is the project's own, or a scratch copy the owner owns (the
// restore test), which the console login is then let into.
func (s *Service) RestoreConn(ctx context.Context, p store.Project, database string) (agentapi.PGConn, error) {
	c, err := s.AgentConn(ctx, p.InstanceID, database)
	if err != nil {
		return c, err
	}
	if err := s.EnsureConsoleRole(ctx, p); err != nil {
		return c, fmt.Errorf("console role: %w", err)
	}
	if database != p.DbName {
		conn, err := s.AdminConn(ctx, p.InstanceID, database)
		if err != nil {
			return c, err
		}
		_, err = conn.Exec(ctx, "GRANT CONNECT ON DATABASE "+ident(database)+" TO "+ident(ConsoleRole(p.DbName)))
		_ = conn.Close(context.Background())
		if err != nil {
			return c, err
		}
	}
	c.User = ConsoleRole(p.DbName)
	c.Password = s.ConsolePassword(c.User)
	return c, nil
}

// RestoreLogin names the short-lived login a promotion or demotion
// restores through (WithRestoreLogin).
func RestoreLogin(dbName string) string { return dbName + "_restore" }

// WithRestoreLogin runs fn with a connection for a pg_restore that keeps
// the dump's owners and grants (promotion, demotion), as a short-lived
// login that inherits and may become each of roles, but is no superuser:
// the dump's functions run during a restore (CHECK constraints, index
// expressions), and must not run as the superuser. Afterwards anything the
// login still owns goes to owner and the login is dropped.
func (s *Service) WithRestoreLogin(ctx context.Context, instanceID uuid.UUID, database, owner string, roles []string, fn func(agentapi.PGConn) error) error {
	c, err := s.AgentConn(ctx, instanceID, database)
	if err != nil {
		return err
	}
	conn, err := s.AdminConn(ctx, instanceID, database)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	login := RestoreLogin(database)
	pw := make([]byte, 24)
	if _, err := rand.Read(pw); err != nil {
		return err
	}
	password := base64.RawURLEncoding.EncodeToString(pw)
	verifier, err := crypto.SCRAMVerifier(password)
	if err != nil {
		return err
	}
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, login).Scan(&exists); err != nil {
		return err
	}
	verb := "CREATE"
	if exists { // left by an interrupted attempt
		verb = "ALTER"
	}
	stmts := []string{
		fmt.Sprintf("%s ROLE %s LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 2 PASSWORD '%s'",
			verb, ident(login), verifier),
		"GRANT CONNECT ON DATABASE " + ident(database) + " TO " + ident(login),
	}
	seen := map[string]bool{}
	for _, r := range append([]string{owner}, roles...) {
		if seen[r] || r == login {
			continue
		}
		seen[r] = true
		stmts = append(stmts, "GRANT "+ident(r)+" TO "+ident(login)+" WITH INHERIT TRUE, SET TRUE")
	}
	defer func() {
		if err := DropRestoreLogin(context.WithoutCancel(ctx), conn, database, owner); err != nil {
			s.log.Warn("drop restore login", "login", login, "err", err)
		}
	}()
	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("restore login: %w", err)
		}
	}
	c.User, c.Password = login, password
	return fn(c)
}

// DropRestoreLogin drops database's restore login, if any, giving what it
// owns there to owner. conn is an admin connection to database.
func DropRestoreLogin(ctx context.Context, conn *pgx.Conn, database, owner string) error {
	login := RestoreLogin(database)
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, login).Scan(&exists); err != nil || !exists {
		return err
	}
	for _, stmt := range []string{
		"REASSIGN OWNED BY " + ident(login) + " TO " + ident(owner),
		"DROP OWNED BY " + ident(login),
		"DROP ROLE " + ident(login),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
