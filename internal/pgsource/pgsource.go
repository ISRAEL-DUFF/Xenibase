// Package pgsource holds what reading from someone else's Postgres has in
// common: an import or a Supabase migration copies a source database, and
// must not change it.
package pgsource

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ReadOnly makes the session read-only with SET, once connected.
//
// It is not a startup parameter on purpose: a PgBouncer in front of the source
// (a Xenibase V2 pooler, or any other) refuses a connection whose startup
// message carries a parameter it doesn't ignore, "FATAL: unsupported startup
// parameter: default_transaction_read_only". A SET works through a pooler in
// session mode and straight to Postgres alike. The copy itself is made by
// pg_dump, which reads in its own read-only snapshot.
func ReadOnly(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, "SET default_transaction_read_only = on"); err != nil {
		return fmt.Errorf("make the source session read-only: %w", err)
	}
	return nil
}
