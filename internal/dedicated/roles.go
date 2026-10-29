package dedicated

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/logical"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Role management on dedicated projects: the owner role gets CREATEROLE,
// so an application whose migrations create roles (Taskiem's
// taskiem_app, its <db>_app login) runs unchanged. Postgres 16 and later
// let a CREATEROLE role alter, drop and grant only the roles it holds
// ADMIN on (those it created), and give no one SUPERUSER, REPLICATION or
// BYPASSRLS it lacks; the instance is the project's alone, so the only
// other roles are Xenibase's, which the owner holds no ADMIN on.
//
// The poolers accept only logins Xenibase knows, so a sweep reads the
// logins the owner administers and records them (project_owned_logins);
// only those named <db>_<something> that aren't Xenibase's own names are
// synced, since each region's pooler auth file is one namespace. The
// sweep also caps their connection limits. A move or major upgrade
// recreates the owner's roles on the target before its copy.

// MinManageRolesMajor is the oldest Postgres whose CREATEROLE is limited
// to the roles a role created.
const MinManageRolesMajor = 16

// scramPrefix starts the only password kind the poolers can use.
const scramPrefix = "SCRAM-SHA-256$"

// platformRoles are the project's own roles: never the owner's to manage,
// never synced from the instance.
func platformRoles(p store.Project) []string {
	out := []string{p.OwnerRole, provision.ReadOnlyRole(p.DbName), provision.ConsoleRole(p.DbName), provision.ConsoleReadOnlyRole(p.DbName),
		provision.RestoreLogin(p.DbName), store.ProbeRole(p.DbName)}
	out = append(out, store.ServiceRoles(p.DbName)...)
	if p.LegacyOwnerRole != nil {
		out = append(out, *p.LegacyOwnerRole)
	}
	return out
}

// platformRole reports whether name is one of p's own roles or of
// Xenibase's generally.
func platformRole(p store.Project, name string) bool {
	return slices.Contains(platformRoles(p), name) || strings.HasPrefix(name, p.DbName+"_u_") ||
		strings.HasPrefix(name, logical.Prefix) || strings.HasPrefix(name, "xenibase") || strings.HasPrefix(name, "pg_")
}

// SetManageRoles turns role management on or off for a dedicated project:
// the owner role gets or loses CREATEROLE. Roles the owner created stay
// either way, and their logins keep working.
func (s *Service) SetManageRoles(ctx context.Context, p store.Project, on bool) (store.Project, error) {
	if p.Tier != provision.TierDedicated {
		return p, fmt.Errorf("%w: role management is for dedicated projects; %s is on the shared tier", provision.ErrConflict, p.Name)
	}
	if p.Status != "active" {
		return p, fmt.Errorf("%w: the project is %s", provision.ErrConflict, p.Status)
	}
	admin, err := s.projects.AdminConn(ctx, p.InstanceID, "postgres")
	if err != nil {
		return p, err
	}
	defer admin.Close(context.Background())
	var major int
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int / 10000`).Scan(&major); err != nil {
		return p, err
	}
	if on && major < MinManageRolesMajor {
		return p, fmt.Errorf("%w: role management needs Postgres %d or later; upgrade the project first", provision.ErrConflict, MinManageRolesMajor)
	}
	out, err := store.New(s.db).SetProjectManageRoles(ctx, store.SetProjectManageRolesParams{ID: p.ID, ManageRoles: on})
	if err != nil {
		return p, err
	}
	if _, err := admin.Exec(ctx, "ALTER ROLE "+provision.Ident(p.OwnerRole)+" "+provision.OwnerCreateRole(out)); err != nil {
		return out, err
	}
	if _, err := s.SyncOwnedRoles(ctx, out); err != nil {
		return out, err
	}
	return out, nil
}

// ownedRole is a role the project's owner administers, as the instance has it.
type ownedRole struct {
	Name                 string
	Login, Inherit       bool
	CreateRole, CreateDB bool
	Super, Repl, Bypass  bool
	ConnLimit            int
	Password             *string
}

// ownedMembership is a grant that involves an owned role.
type ownedMembership struct {
	Role, Member              string
	Admin, Inherit, SetOption bool
}

// ownedRoles reads the roles p's owner administers (ADMIN OPTION), apart
// from the project's own.
func ownedRoles(ctx context.Context, admin *pgx.Conn, p store.Project) ([]ownedRole, error) {
	rows, err := admin.Query(ctx, `SELECT a.rolname, a.rolcanlogin, a.rolinherit, a.rolcreaterole, a.rolcreatedb,
			a.rolsuper, a.rolreplication, a.rolbypassrls, a.rolconnlimit, a.rolpassword
		FROM pg_auth_members m JOIN pg_authid a ON a.oid = m.roleid
		WHERE m.member = (SELECT oid FROM pg_roles WHERE rolname = $1) AND m.admin_option
		ORDER BY a.rolname`, p.OwnerRole)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ownedRole, error) {
		var o ownedRole
		err := r.Scan(&o.Name, &o.Login, &o.Inherit, &o.CreateRole, &o.CreateDB, &o.Super, &o.Repl, &o.Bypass, &o.ConnLimit, &o.Password)
		return o, err
	})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(o ownedRole) bool { return platformRole(p, o.Name) }), nil
}

// syncable reports why an owned login can't be synced to the poolers, or "".
func syncable(p store.Project, o ownedRole) string {
	switch {
	case !o.Login:
		return "not a login"
	case !strings.HasPrefix(o.Name, p.DbName+"_") || len(o.Name) > 63 || strings.Trim(o.Name, "abcdefghijklmnopqrstuvwxyz0123456789_") != "":
		return fmt.Sprintf("not named %s_<lowercase letters, digits, _>", p.DbName)
	case o.Super || o.Repl || o.Bypass:
		return "has SUPERUSER, REPLICATION or BYPASSRLS"
	case o.Password == nil || !strings.HasPrefix(*o.Password, scramPrefix) || !crypto.IsSCRAMVerifier(*o.Password):
		return "has no SCRAM-SHA-256 password"
	}
	return ""
}

// OwnedRolesResult is what a sync found.
type OwnedRolesResult struct {
	// Synced are the logins the poolers accept.
	Synced []string
	// Skipped are the owner's other roles, with why they aren't synced.
	Skipped map[string]string
	// Changed: the synced set differs from before (the poolers reloaded).
	Changed bool
}

// SyncOwnedRoles reads the roles p's owner administers, caps its logins'
// connection limits at the project's, and records the logins the poolers
// should accept, reloading them when that set changed.
func (s *Service) SyncOwnedRoles(ctx context.Context, p store.Project) (OwnedRolesResult, error) {
	res := OwnedRolesResult{Synced: []string{}, Skipped: map[string]string{}}
	set, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		return res, err
	}
	admin, err := s.projects.AdminConn(ctx, p.InstanceID, "postgres")
	if err != nil {
		return res, err
	}
	defer admin.Close(context.Background())
	owned, err := ownedRoles(ctx, admin, p)
	if err != nil {
		return res, err
	}
	want := map[string]string{}
	for _, o := range owned {
		if o.Login && (o.ConnLimit < 0 || o.ConnLimit > set.ConnectionLimit) {
			// V2 §10.4's limit, which the owner could lift again: the next
			// sweep puts it back.
			if _, err := admin.Exec(ctx, fmt.Sprintf("ALTER ROLE %s CONNECTION LIMIT %d", provision.Ident(o.Name), set.ConnectionLimit)); err != nil {
				return res, fmt.Errorf("cap %s: %w", o.Name, err)
			}
		}
		if why := syncable(p, o); why != "" {
			if o.Login {
				res.Skipped[o.Name] = why
			}
			continue
		}
		// Project databases don't grant CONNECT to PUBLIC, and the owner
		// can't grant it: a synced login is one of the project's.
		if err := grantConnect(ctx, admin, p.DbName, o.Name); err != nil {
			return res, err
		}
		want[o.Name] = *o.Password
	}
	q := store.New(s.db)
	have, err := q.OwnedLoginVerifiers(ctx, p.ID)
	if err != nil {
		return res, err
	}
	same := len(have) == len(want)
	for _, h := range have {
		same = same && want[h.RoleName] == h.ScramVerifier
	}
	if !same {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return res, err
		}
		defer tx.Rollback(context.Background()) //nolint:errcheck // after commit, a no-op
		qt := store.New(tx)
		if err := qt.DeleteOwnedLogins(ctx, p.ID); err != nil {
			return res, err
		}
		for name, v := range want {
			if err := qt.InsertOwnedLogin(ctx, store.InsertOwnedLoginParams{ProjectID: p.ID, RoleName: name, ScramVerifier: v}); err != nil {
				return res, fmt.Errorf("record %s: %w", name, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return res, err
		}
		res.Changed = true
		if pm := s.projects.Pooler(); pm != nil {
			if err := pm.Sync(ctx); err != nil {
				return res, fmt.Errorf("pooler sync: %w", err)
			}
		}
	}
	for name := range want {
		res.Synced = append(res.Synced, name)
	}
	slices.Sort(res.Synced)
	return res, nil
}

// RunOwnedRoles syncs every project with role management on every
// interval until ctx ends, so a login the owner creates can connect
// through the poolers within that time.
func (s *Service) RunOwnedRoles(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ps, err := store.New(s.db).ManageRolesProjects(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("owned roles sweep", "err", err)
			}
			continue
		}
		for _, p := range ps {
			if _, err := s.SyncOwnedRoles(ctx, p); err != nil && ctx.Err() == nil {
				s.log.Warn("owned roles sweep", "project", p.ID, "err", err)
			}
		}
	}
}

// restoreOwnedRoles recreates, on pt's instance, the roles p's owner
// administers on p's: same attributes and password, the owner's ADMIN
// OPTION, and the memberships between them and the project's roles.
// Before the copy, so grants and ownership in the dump find them.
func (s *Service) restoreOwnedRoles(ctx context.Context, p, pt store.Project, log *jobs.StepLogger) error {
	src, err := s.projects.AdminConn(ctx, p.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer src.Close(context.Background())
	owned, err := ownedRoles(ctx, src, p)
	if err != nil || len(owned) == 0 {
		return err
	}
	names := make([]string, len(owned))
	for i, o := range owned {
		names[i] = o.Name
	}
	rows, err := src.Query(ctx, `SELECT g.rolname, u.rolname, m.admin_option, m.inherit_option, m.set_option
		FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles u ON u.oid = m.member
		WHERE g.rolname = ANY($1) OR u.rolname = ANY($1) ORDER BY 1, 2`, names)
	if err != nil {
		return err
	}
	grants, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ownedMembership, error) {
		var m ownedMembership
		err := r.Scan(&m.Role, &m.Member, &m.Admin, &m.Inherit, &m.SetOption)
		return m, err
	})
	if err != nil {
		return err
	}
	dst, err := s.projects.AdminConn(ctx, pt.InstanceID, "postgres")
	if err != nil {
		return err
	}
	defer dst.Close(context.Background())
	if err := applyOwnedRoles(ctx, dst, p.OwnerRole, pt.OwnerRole, owned, grants); err != nil {
		return err
	}
	for _, o := range owned {
		if syncable(p, o) == "" {
			if err := grantConnect(ctx, dst, pt.DbName, o.Name); err != nil {
				return err
			}
		}
	}
	return log.Info(ctx, "roles", "the owner's %d role(s) recreated on the target: %s", len(owned), strings.Join(names, ", "))
}

// grantConnect lets login connect to db, if it can't yet.
func grantConnect(ctx context.Context, conn roleConn, db, login string) error {
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT has_database_privilege($1, $2, 'CONNECT')`, login, db).Scan(&ok); err != nil {
		return err
	}
	if ok {
		return nil
	}
	if _, err := conn.Exec(ctx, "GRANT CONNECT ON DATABASE "+provision.Ident(db)+" TO "+provision.Ident(login)); err != nil {
		return fmt.Errorf("connect for %s: %w", login, err)
	}
	return nil
}

// applyOwnedRoles creates owned on dst (or brings an existing one's
// attributes and password in line) and re-grants grants, with srcOwner
// read as dstOwner. A grant whose other role isn't on dst is skipped.
func applyOwnedRoles(ctx context.Context, dst roleConn, srcOwner, dstOwner string, owned []ownedRole, grants []ownedMembership) error {
	yes := func(b bool, on, off string) string {
		if b {
			return on
		}
		return off
	}
	for _, o := range owned {
		if o.Super || o.Repl || o.Bypass {
			return fmt.Errorf("%w: role %s has SUPERUSER, REPLICATION or BYPASSRLS, which the owner can't have given it", provision.ErrConflict, o.Name)
		}
		attrs := strings.Join([]string{yes(o.Login, "LOGIN", "NOLOGIN"), yes(o.Inherit, "INHERIT", "NOINHERIT"),
			yes(o.CreateRole, "CREATEROLE", "NOCREATEROLE"), yes(o.CreateDB, "CREATEDB", "NOCREATEDB"),
			"NOSUPERUSER NOREPLICATION NOBYPASSRLS", fmt.Sprintf("CONNECTION LIMIT %d", o.ConnLimit)}, " ")
		if o.Password != nil {
			if strings.Contains(*o.Password, "'") {
				return fmt.Errorf("%w: role %s has an unusable password", provision.ErrInvalid, o.Name)
			}
			attrs += " PASSWORD '" + *o.Password + "'"
		}
		var exists bool
		if err := dst.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, o.Name).Scan(&exists); err != nil {
			return err
		}
		verb := "CREATE"
		if exists {
			verb = "ALTER"
		}
		if _, err := dst.Exec(ctx, verb+" ROLE "+provision.Ident(o.Name)+" "+attrs); err != nil {
			return fmt.Errorf("role %s: %w", o.Name, err)
		}
	}
	for _, g := range grants {
		role, member := g.Role, g.Member
		if role == srcOwner {
			role = dstOwner
		}
		if member == srcOwner {
			member = dstOwner
		}
		var both bool
		if err := dst.QueryRow(ctx, `SELECT count(*) = 2 FROM pg_roles WHERE rolname IN ($1, $2)`, role, member).Scan(&both); err != nil {
			return err
		}
		if !both {
			continue
		}
		stmt := fmt.Sprintf("GRANT %s TO %s WITH ADMIN %t, INHERIT %t, SET %t", provision.Ident(role), provision.Ident(member), g.Admin, g.Inherit, g.SetOption)
		if _, err := dst.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("grant %s to %s: %w", role, member, err)
		}
	}
	return nil
}
