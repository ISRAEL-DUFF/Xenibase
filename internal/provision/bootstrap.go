package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/store"
)

// SharedCluster describes a shared-tier Postgres cluster to register at
// startup. Until agents arrive (M4), this is how the first node is added.
type SharedCluster struct {
	// NodeName is the node's unique name, e.g. "local".
	NodeName string
	// AdminURL is how xenibase-server connects to the cluster as a superuser
	// (xenibase_admin).
	AdminURL string
	// PoolerHost and PoolerPort are how the poolers reach the cluster; they
	// default to AdminURL's host and port.
	PoolerHost string
	PoolerPort int
	// NodeRole is the node's role when first registered (default "both");
	// an existing node keeps the role it has.
	NodeRole string
}

// RegisterSharedCluster verifies and hardens a shared cluster, then records
// it as a node with one shared instance. It is idempotent and runs on every
// start, which also picks up a changed admin password.
func RegisterSharedCluster(ctx context.Context, db *pgxpool.Pool, keyring *crypto.Keyring, c SharedCluster, log *slog.Logger) error {
	cfg, err := pgx.ParseConfig(c.AdminURL)
	if err != nil {
		return fmt.Errorf("shared cluster admin url: %w", err)
	}
	if c.NodeRole == "" {
		c.NodeRole = "both"
	}
	if c.PoolerHost == "" {
		c.PoolerHost = cfg.Host
	}
	if c.PoolerPort == 0 {
		c.PoolerPort = int(cfg.Port)
	}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect to shared cluster: %w", err)
	}
	defer conn.Close(context.Background())

	var super bool
	var versionNum string
	if err := conn.QueryRow(ctx,
		`SELECT rolsuper, current_setting('server_version_num') FROM pg_roles WHERE rolname = current_user`,
	).Scan(&super, &versionNum); err != nil {
		return fmt.Errorf("inspect shared cluster: %w", err)
	}
	if !super {
		return errors.New("shared cluster: the admin role must be a superuser (xenibase_admin)")
	}
	vn, _ := strconv.Atoi(versionNum)
	major := vn / 10000

	if err := hardenCluster(ctx, conn, log); err != nil {
		return err
	}

	if !pooler.ValidHost(c.PoolerHost) {
		return fmt.Errorf("XENIBASE_SHARED_POOLER_HOST %q: the poolers route to it, so it must be a host name (letters, digits, dots and dashes) or an IP address", c.PoolerHost)
	}
	q := store.New(db)
	node, err := q.UpsertNode(ctx, store.UpsertNodeParams{
		Name: c.NodeName, PrivateAddr: c.PoolerHost, Role: c.NodeRole,
	})
	if err != nil {
		return fmt.Errorf("register node: %w", err)
	}
	sealed, err := sealAdminSecret(keyring, node.ID, AdminSecret{User: cfg.User, Password: cfg.Password})
	if err != nil {
		return err
	}
	if err := q.SetNodeAdminSecret(ctx, store.SetNodeAdminSecretParams{ID: node.ID, PgAdminSecret: sealed}); err != nil {
		return fmt.Errorf("store node admin secret: %w", err)
	}

	adminHost, adminPort := cfg.Host, int32(cfg.Port)
	inst, err := q.UpsertSharedInstance(ctx, store.UpsertSharedInstanceParams{
		NodeID: node.ID, PgVersion: int32(major), Port: int32(c.PoolerPort),
		AdminHost: &adminHost, AdminPort: &adminPort,
	})
	if err != nil {
		return fmt.Errorf("register shared instance: %w", err)
	}
	log.Info("shared cluster registered", "node", c.NodeName, "instance_id", inst.ID,
		"pg_version", major, "pooler_backend", fmt.Sprintf("%s:%d", c.PoolerHost, c.PoolerPort))
	return nil
}

// hardenCluster applies cluster-wide isolation settings (spec §7.1) and
// warns about configuration only the operator can change.
func hardenCluster(ctx context.Context, conn *pgx.Conn, log *slog.Logger) error {
	// Project roles must not reach the maintenance databases. (CREATE
	// DATABASE copies template1 without connecting to it.)
	for _, stmt := range []string{
		`REVOKE CONNECT, TEMPORARY ON DATABASE postgres FROM PUBLIC`,
		`REVOKE CONNECT, TEMPORARY ON DATABASE template1 FROM PUBLIC`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("harden shared cluster: %s: %w", stmt, err)
		}
	}

	var logStatement string
	if err := conn.QueryRow(ctx, `SELECT current_setting('log_statement')`).Scan(&logStatement); err == nil && logStatement != "none" {
		log.Warn("shared cluster has log_statement enabled; project SQL may contain secrets (spec §7.1)", "log_statement", logStatement)
	}
	rows, err := conn.Query(ctx, `
		SELECT line_number, type, auth_method FROM pg_hba_file_rules
		WHERE type <> 'local' AND auth_method NOT IN ('scram-sha-256', 'reject', 'cert')`)
	if err == nil {
		for rows.Next() {
			var line int
			var typ, method string
			if rows.Scan(&line, &typ, &method) == nil {
				log.Warn("shared cluster pg_hba.conf allows network logins without scram-sha-256 (spec §7.1)",
					"line", line, "type", typ, "auth_method", method)
			}
		}
		rows.Close()
	}
	return nil
}
