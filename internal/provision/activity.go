package provision

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// activityPrivacy hides other sessions and other databases' activity from
// tenants on a shared cluster (V2 §10.2): pg_stat_activity and the
// cluster-wide pg_stat_database views, and the functions behind them, which
// return any backend's database and role, and any database's commits, rows
// and blocks, without a privilege check of their own. Only superusers
// (Xenibase's admin) read them afterwards. Catalog ACLs are per database,
// so this runs in every project database.
var activityPrivacy = []string{
	"REVOKE SELECT ON pg_catalog.pg_stat_activity FROM PUBLIC",
	"REVOKE SELECT ON pg_catalog.pg_stat_database FROM PUBLIC",
	"REVOKE SELECT ON pg_catalog.pg_stat_database_conflicts FROM PUBLIC",
}

// ActivityFunctions selects the statistics functions activityPrivacy
// revokes, as regprocedure signatures: pg_stat_get_activity, every
// pg_stat_get_backend_* and pg_stat_get_db_* (the set differs between
// majors), and pg_stat_get_progress_info, which names other backends'
// databases.
const ActivityFunctions = `SELECT p.oid::regprocedure::text FROM pg_catalog.pg_proc p
	WHERE p.pronamespace = 'pg_catalog'::regnamespace
	  AND (p.proname IN ('pg_stat_get_activity', 'pg_stat_get_progress_info')
	       OR p.proname LIKE 'pg\_stat\_get\_backend\_%' OR p.proname LIKE 'pg\_stat\_get\_db\_%')
	ORDER BY 1`

// RestrictActivity applies the activity restriction in conn's database.
// It is idempotent, so it also brings databases provisioned by an older
// release up to date (apply_settings runs it).
func RestrictActivity(ctx context.Context, conn *pgx.Conn) error {
	rows, err := conn.Query(ctx, ActivityFunctions)
	if err != nil {
		return fmt.Errorf("restrict pg_stat_activity: %w", err)
	}
	fns, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("restrict pg_stat_activity: %w", err)
	}
	stmts := append([]string(nil), activityPrivacy...)
	for _, fn := range fns {
		// regprocedure prints pg_catalog's functions unqualified (the
		// schema is always on the path), with their argument types.
		stmts = append(stmts, "REVOKE EXECUTE ON FUNCTION pg_catalog."+fn+" FROM PUBLIC")
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("restrict pg_stat_activity: %w", err)
		}
	}
	return nil
}
