// Package pgsource holds what reading from someone else's Postgres has in
// common: an import or a Supabase migration copies a source database, and
// must not change it.
package pgsource

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentapi"
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

// Guard checks a source's host and port before anything connects to it,
// and returns the address to connect to (outbound.Service.CheckSource).
type Guard func(ctx context.Context, host string, port int) (netip.Addr, error)

// Config parses raw, a tenant's postgres:// URL, into a pgx config and
// the agent's PGConn, both pinned to the address guard approved: pgx
// resolves every host to it, and the agent gets it as hostaddr. A URL
// whose pgx settings name another host or port than its authority (a
// host= parameter, several hosts, a socket directory) is refused, so the
// address checked is the one used.
func Config(ctx context.Context, raw string, guard Guard) (*pgx.ConnConfig, agentapi.PGConn, error) {
	pg, err := agentapi.ParseURL(raw)
	if err != nil {
		return nil, pg, err
	}
	cfg, err := pgx.ParseConfig(strings.TrimSpace(raw))
	if err != nil {
		return nil, pg, err
	}
	hosts := []struct {
		host string
		port uint16
	}{{cfg.Host, cfg.Port}}
	for _, fb := range cfg.Fallbacks {
		hosts = append(hosts, struct {
			host string
			port uint16
		}{fb.Host, fb.Port})
	}
	for _, h := range hosts {
		if !strings.EqualFold(h.host, pg.Host) || int(h.port) != pg.Port {
			return nil, pg, fmt.Errorf("the connection string must name one host and port, without host= or hostaddr= parameters")
		}
	}
	if guard == nil {
		return nil, pg, errors.New("no address check is configured for database sources")
	}
	addr, err := guard(ctx, pg.Host, pg.Port)
	if err != nil {
		return nil, pg, err
	}
	pinned := addr.String()
	cfg.LookupFunc = func(context.Context, string) ([]string, error) { return []string{pinned}, nil }
	pg.HostAddr = pinned
	return cfg, pg, nil
}
