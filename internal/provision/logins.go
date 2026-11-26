package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
)

// TempFileLimit caps the temporary files one session of a shared-tier
// project may write (V2 §10.4). Only a superuser can change it, so a tenant
// cannot raise it.
const TempFileLimit = "2GB"

func tempFileLimit(role, tier string) string {
	if tier == TierShared {
		return "ALTER ROLE " + ident(role) + " SET temp_file_limit = " + literal(TempFileLimit)
	}
	return "ALTER ROLE " + ident(role) + " RESET temp_file_limit"
}

// loginAttr is LOGIN, or NOLOGIN while the project's logins are switched
// off (a hard storage lock or a suspended organisation), so re-applying
// settings or issuing credentials never undoes a lock.
func (s *Service) loginAttr(ctx context.Context, p store.Project) (string, error) {
	if s.LoginGate == nil {
		return "LOGIN", nil
	}
	ok, err := s.LoginGate(ctx, p)
	if err != nil {
		return "", err
	}
	if ok {
		return "LOGIN", nil
	}
	return "NOLOGIN", nil
}

// ProjectLogins are the login roles apps and members use for p: the owner,
// the V1 owner during a switch to opaque credentials, personal logins, the
// logins the owner of a dedicated project with role management created
// (project_owned_logins), and the backend services' logins.
// The console role is not one of them: it keeps working through locks so
// a team can clean up (V2 §10.4).
func (s *Service) ProjectLogins(ctx context.Context, p store.Project) ([]string, error) {
	roles := []string{p.OwnerRole}
	if p.LegacyOwnerRole != nil {
		roles = append(roles, *p.LegacyOwnerRole)
	}
	q := store.New(s.db)
	users, err := q.ListProjectDBUsers(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		roles = append(roles, u.RoleName)
	}
	owned, err := q.ListOwnedLogins(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for _, o := range owned {
		roles = append(roles, o.RoleName)
	}
	// Backend services' logins (V4 §2.3), when there are some.
	roles = append(roles, store.ServiceRoles(p.DbName)...)
	return roles, nil
}

// SetLogins switches p's logins on or off. Switching off ends their
// sessions, on the poolers and the server, so the change holds at once.
func (s *Service) SetLogins(ctx context.Context, p store.Project, allowed bool) error {
	roles, err := s.ProjectLogins(ctx, p)
	if err != nil {
		return err
	}
	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	attr := "LOGIN"
	if !allowed {
		attr = "NOLOGIN"
	}
	var errs []error
	for _, r := range roles {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, r).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := conn.Exec(ctx, "ALTER ROLE "+ident(r)+" "+attr); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", attr, r, err))
		}
	}
	if !allowed {
		for _, r := range roles {
			if s.pooler != nil {
				if err := s.pooler.KillUser(ctx, r); err != nil {
					errs = append(errs, err)
				}
			}
		}
		if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = ANY($1)`, roles); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SetReadOnly sets or clears p's database-level default read-only mode (a
// soft storage lock, V2 §10.4). With endIdle, setting it also ends the
// database's idle sessions, so reconnecting clients pick it up.
func (s *Service) SetReadOnly(ctx context.Context, p store.Project, on, endIdle bool) error {
	conn, err := s.connectInstance(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	stmt := "ALTER DATABASE " + ident(p.DbName) + " RESET default_transaction_read_only"
	if on {
		stmt = "ALTER DATABASE " + ident(p.DbName) + " SET default_transaction_read_only = on"
	}
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return err
	}
	if on && endIdle {
		_, err = conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			WHERE datname = $1 AND state = 'idle' AND usename <> ALL($2) AND pid <> pg_backend_pid()`, p.DbName, []string{ConsoleRole(p.DbName), ConsoleReadOnlyRole(p.DbName)})
	}
	return err
}

// LargestTables lists p's biggest tables (for Reclaim space).
func (s *Service) LargestTables(ctx context.Context, p store.Project, limit int) ([]TableSize, error) {
	conn, err := s.connectInstance(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `
		SELECT n.nspname, c.relname, pg_total_relation_size(c.oid), COALESCE(s.n_dead_tup, 0)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
		WHERE c.relkind IN ('r', 'm', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%'
		ORDER BY 3 DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TableSize, error) {
		var t TableSize
		err := r.Scan(&t.Schema, &t.Table, &t.Bytes, &t.DeadRows)
		return t, err
	})
}

// TableSize is one table's footprint.
type TableSize struct {
	Schema, Table string
	Bytes         int64
	DeadRows      int64
}
