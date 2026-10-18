package dedicated

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Integrator roles: NOLOGIN roles an operator creates on a dedicated
// project's instance for an integrator whose migrations expect them
// (Taskiem's taskiem_app and taskiem_dispatch). Project owners are
// NOCREATEROLE, so only the platform can make them; the owner gets each
// WITH ADMIN OPTION and manages membership from there. They are recorded
// on the project, so a move recreates them on the target and the
// demotion check and the isolation audit know what they are.

// MaxIntegratorRoles a project may have.
const MaxIntegratorRoles = 8

var integratorRoleRe = regexp.MustCompile(`^[a-z][a-z0-9_]{2,62}$`)

// reservedRoleNames can't be integrator roles: Postgres's own, and the
// Supabase API roles imports recreate (internal/isocheck GenericRoles).
var reservedRoleNames = []string{"postgres", "public", "anon", "authenticated", "service_role"}

// ValidateIntegratorRoles checks a list of role names: 1 to 8 distinct
// names of ^[a-z][a-z0-9_]{2,62}$, none reserved.
func ValidateIntegratorRoles(names []string) error {
	if len(names) == 0 || len(names) > MaxIntegratorRoles {
		return fmt.Errorf("%w: give 1 to %d role names", provision.ErrInvalid, MaxIntegratorRoles)
	}
	for i, n := range names {
		switch {
		case !integratorRoleRe.MatchString(n):
			return fmt.Errorf("%w: role %q must match ^[a-z][a-z0-9_]{2,62}$", provision.ErrInvalid, n)
		case strings.HasPrefix(n, "pg_"), strings.HasPrefix(n, "xenibase"), slices.Contains(reservedRoleNames, n):
			return fmt.Errorf("%w: role %q is reserved", provision.ErrInvalid, n)
		case slices.Contains(names[:i], n):
			return fmt.Errorf("%w: role %q is listed twice", provision.ErrInvalid, n)
		}
	}
	return nil
}

// IntegratorRoleStatements are what the instance's superuser runs for one
// role: create it with no attribute beyond membership, and hand it to the
// project's owner WITH ADMIN OPTION (the owner can grant and revoke it,
// and SET ROLE to it).
func IntegratorRoleStatements(role, owner string) []string {
	return []string{
		"CREATE ROLE " + provision.Ident(role) + " NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"GRANT " + provision.Ident(role) + " TO " + provision.Ident(owner) + " WITH ADMIN OPTION",
	}
}

// roleConn is what creating roles needs of an admin connection.
type roleConn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// createIntegratorRoles creates the missing roles and (re)grants each to
// owner. It returns the ones it created.
func createIntegratorRoles(ctx context.Context, conn roleConn, owner string, names []string) ([]string, error) {
	var created []string
	for _, n := range names {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, n).Scan(&exists); err != nil {
			return created, err
		}
		stmts := IntegratorRoleStatements(n, owner)
		if exists {
			stmts = stmts[1:]
		}
		for _, stmt := range stmts {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return created, fmt.Errorf("role %s: %w", n, err)
			}
		}
		if !exists {
			created = append(created, n)
		}
	}
	return created, nil
}

// IntegratorRolesResult is what CreateIntegratorRoles did.
type IntegratorRolesResult struct {
	// Roles is every integrator role recorded on the project.
	Roles []string
	// Created is the ones this call created (the rest already existed).
	Created []string
}

// CreateIntegratorRoles creates names on p's instance as the superuser,
// grants them to p's owner role WITH ADMIN OPTION, and records them on
// the project. Dedicated projects only.
func (s *Service) CreateIntegratorRoles(ctx context.Context, p store.Project, names []string) (IntegratorRolesResult, error) {
	if err := ValidateIntegratorRoles(names); err != nil {
		return IntegratorRolesResult{}, err
	}
	if p.Tier != provision.TierDedicated {
		return IntegratorRolesResult{}, fmt.Errorf("%w: dedicated projects only; %s is on the shared tier", provision.ErrConflict, p.Name)
	}
	own := []string{p.OwnerRole, provision.ReadOnlyRole(p.DbName), provision.ConsoleRole(p.DbName), provision.RestoreLogin(p.DbName)}
	if p.LegacyOwnerRole != nil {
		own = append(own, *p.LegacyOwnerRole)
	}
	for _, n := range names {
		if slices.Contains(own, n) || strings.HasPrefix(n, p.DbName+"_") {
			return IntegratorRolesResult{}, fmt.Errorf("%w: %s is one of the project's own roles", provision.ErrInvalid, n)
		}
	}
	admin, err := s.projects.AdminConn(ctx, p.InstanceID, "postgres")
	if err != nil {
		return IntegratorRolesResult{}, err
	}
	defer admin.Close(context.Background())
	created, err := createIntegratorRoles(ctx, admin, p.OwnerRole, names)
	if err != nil {
		return IntegratorRolesResult{}, err
	}
	roles := slices.Clone(p.IntegratorRoles)
	for _, n := range names {
		if !slices.Contains(roles, n) {
			roles = append(roles, n)
		}
	}
	slices.Sort(roles)
	if err := store.New(s.db).SetProjectIntegratorRoles(ctx, store.SetProjectIntegratorRolesParams{ID: p.ID, IntegratorRoles: roles}); err != nil {
		return IntegratorRolesResult{}, err
	}
	if created == nil {
		created = []string{}
	}
	return IntegratorRolesResult{Roles: roles, Created: created}, nil
}

// restoreIntegratorRoles recreates p's integrator roles on pt's instance
// before its database is copied there, so a move keeps them with the
// owner's admin option rather than as bare NOLOGIN stand-ins.
func (s *Service) restoreIntegratorRoles(ctx context.Context, p, pt store.Project, log *jobs.StepLogger) error {
	if len(p.IntegratorRoles) == 0 {
		return nil
	}
	admin, err := s.projects.AdminConn(ctx, pt.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	created, err := createIntegratorRoles(ctx, admin, pt.OwnerRole, p.IntegratorRoles)
	if err != nil {
		return err
	}
	return log.Info(ctx, "roles", "integrator roles %v on the target (%d created), granted to the owner with admin option", p.IntegratorRoles, len(created))
}
