package pooler

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Admin talks to one PgBouncer's admin console (the virtual "pgbouncer"
// database), which only speaks the simple query protocol.
type Admin struct {
	Name string // for logs, e.g. "session" or "transaction"
	cfg  *pgx.ConnConfig
}

// NewAdmin returns a client for the admin console of the PgBouncer at addr
// (host:port), logging in as an admin_users entry.
func NewAdmin(name, addr, user, password, sslmode string) (*Admin, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("pooler %s address %q: %w", name, addr, err)
	}
	cfg, err := pgx.ParseConfig(fmt.Sprintf("host=%s port=%s sslmode=%s", host, port, sslmode))
	if err != nil {
		return nil, fmt.Errorf("pooler %s: %w", name, err)
	}
	cfg.User, cfg.Password, cfg.Database = user, password, "pgbouncer"
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	// The admin console rejects unknown startup parameters.
	delete(cfg.RuntimeParams, "application_name")
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}
	return &Admin{Name: name, cfg: cfg}, nil
}

// Addr returns host:port of the pooler.
func (a *Admin) Addr() string { return fmt.Sprintf("%s:%d", a.cfg.Host, a.cfg.Port) }

// Reload makes PgBouncer re-read its configuration and auth files.
func (a *Admin) Reload(ctx context.Context) error { return a.exec(ctx, "RELOAD") }

// Pause waits for server connections to db to be released and holds new
// queries until Resume, so a backend can be switched without client errors.
func (a *Admin) Pause(ctx context.Context, db string) error { return a.dbCommand(ctx, "PAUSE", db) }

// Resume releases a database paused with Pause (or Kill).
func (a *Admin) Resume(ctx context.Context, db string) error { return a.dbCommand(ctx, "RESUME", db) }

// Reconnect closes db's server connections as they are released, so new
// ones pick up changed role settings (ALTER ROLE ... SET applies only to new
// backends).
func (a *Admin) Reconnect(ctx context.Context, db string) error {
	return a.dbCommand(ctx, "RECONNECT", db)
}

// Kill immediately drops all client and server connections to db. New
// clients wait until Resume or until the route is removed.
func (a *Admin) Kill(ctx context.Context, db string) error { return a.dbCommand(ctx, "KILL", db) }

var dbNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func (a *Admin) dbCommand(ctx context.Context, cmd, db string) error {
	if !dbNameRe.MatchString(db) {
		return fmt.Errorf("pooler: invalid database name %q", db)
	}
	return a.exec(ctx, cmd+" "+db)
}

func (a *Admin) exec(ctx context.Context, cmd string) error {
	conn, err := pgx.ConnectConfig(ctx, a.cfg)
	if err != nil {
		return fmt.Errorf("pooler %s (%s): connect admin console: %w", a.Name, a.Addr(), err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, cmd); err != nil {
		return fmt.Errorf("pooler %s (%s): %s: %w", a.Name, a.Addr(), cmd, err)
	}
	return nil
}

// Pool is one row of SHOW POOLS.
type Pool struct {
	Database  string
	User      string
	ClActive  int64
	ClWaiting int64
	SvActive  int64
	SvIdle    int64
}

// Pools runs SHOW POOLS.
func (a *Admin) Pools(ctx context.Context) ([]Pool, error) {
	conn, err := pgx.ConnectConfig(ctx, a.cfg)
	if err != nil {
		return nil, fmt.Errorf("pooler %s (%s): connect admin console: %w", a.Name, a.Addr(), err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, "SHOW POOLS")
	if err != nil {
		return nil, fmt.Errorf("pooler %s: SHOW POOLS: %w", a.Name, err)
	}
	defer rows.Close()
	// Columns vary between PgBouncer versions; read them by name.
	col := map[string]int{}
	for i, fd := range rows.FieldDescriptions() {
		col[fd.Name] = i
	}
	get := func(raw [][]byte, name string) string {
		if i, ok := col[name]; ok && i < len(raw) {
			return string(raw[i])
		}
		return ""
	}
	num := func(raw [][]byte, name string) int64 {
		n, _ := strconv.ParseInt(get(raw, name), 10, 64)
		return n
	}
	var out []Pool
	for rows.Next() {
		raw := rows.RawValues()
		out = append(out, Pool{
			Database: get(raw, "database"), User: get(raw, "user"),
			ClActive: num(raw, "cl_active"), ClWaiting: num(raw, "cl_waiting"),
			SvActive: num(raw, "sv_active"), SvIdle: num(raw, "sv_idle"),
		})
	}
	return out, rows.Err()
}

// Databases runs SHOW DATABASES and returns each route's backend database
// by its client-facing name.
func (a *Admin) Databases(ctx context.Context) (map[string]string, error) {
	conn, err := pgx.ConnectConfig(ctx, a.cfg)
	if err != nil {
		return nil, fmt.Errorf("pooler %s (%s): connect admin console: %w", a.Name, a.Addr(), err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, fmt.Errorf("pooler %s: SHOW DATABASES: %w", a.Name, err)
	}
	defer rows.Close()
	col := map[string]int{}
	for i, fd := range rows.FieldDescriptions() {
		col[fd.Name] = i
	}
	out := map[string]string{}
	for rows.Next() {
		raw := rows.RawValues()
		ni, di := col["name"], col["database"]
		if ni < len(raw) && di < len(raw) {
			out[string(raw[ni])] = string(raw[di])
		}
	}
	return out, rows.Err()
}

// TransferBytes runs SHOW STATS and returns the bytes each database has
// received and sent since the pooler started.
func (a *Admin) TransferBytes(ctx context.Context) (map[string]int64, error) {
	return a.statsSum(ctx, "total_received", "total_sent")
}

// Transactions runs SHOW STATS and returns the transactions and queries
// each database has served since the pooler started: a client that came
// and went between two samples still moves it (V3 §4.2).
func (a *Admin) Transactions(ctx context.Context) (map[string]int64, error) {
	return a.statsSum(ctx, "total_xact_count", "total_query_count")
}

// statsSum sums SHOW STATS columns per database.
func (a *Admin) statsSum(ctx context.Context, cols ...string) (map[string]int64, error) {
	conn, err := pgx.ConnectConfig(ctx, a.cfg)
	if err != nil {
		return nil, fmt.Errorf("pooler %s (%s): connect admin console: %w", a.Name, a.Addr(), err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, "SHOW STATS")
	if err != nil {
		return nil, fmt.Errorf("pooler %s: SHOW STATS: %w", a.Name, err)
	}
	defer rows.Close()
	col := map[string]int{}
	for i, fd := range rows.FieldDescriptions() {
		col[fd.Name] = i
	}
	num := func(raw [][]byte, name string) int64 {
		if i, ok := col[name]; ok && i < len(raw) {
			n, _ := strconv.ParseInt(string(raw[i]), 10, 64)
			return n
		}
		return 0
	}
	out := map[string]int64{}
	for rows.Next() {
		raw := rows.RawValues()
		if i, ok := col["database"]; ok && i < len(raw) {
			for _, c := range cols {
				out[string(raw[i])] += num(raw, c)
			}
		}
	}
	return out, rows.Err()
}

// Setting returns one PgBouncer setting's current value (SHOW CONFIG).
func (a *Admin) Setting(ctx context.Context, key string) (string, error) {
	conn, err := pgx.ConnectConfig(ctx, a.cfg)
	if err != nil {
		return "", fmt.Errorf("pooler %s (%s): connect admin console: %w", a.Name, a.Addr(), err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, "SHOW CONFIG")
	if err != nil {
		return "", fmt.Errorf("pooler %s: SHOW CONFIG: %w", a.Name, err)
	}
	defer rows.Close()
	ki, vi := -1, -1
	for i, fd := range rows.FieldDescriptions() {
		switch fd.Name {
		case "key":
			ki = i
		case "value":
			vi = i
		}
	}
	if ki < 0 || vi < 0 {
		return "", fmt.Errorf("pooler %s: SHOW CONFIG has no key and value columns", a.Name)
	}
	for rows.Next() {
		raw := rows.RawValues()
		if ki < len(raw) && vi < len(raw) && string(raw[ki]) == key {
			return string(raw[vi]), nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("pooler %s: SHOW CONFIG has no %s", a.Name, key)
}

// EnforcesHBA checks that the PgBouncer applies hba.conf (auth_type =
// hba). One with auth_type = scram-sha-256 (the pgbouncer.ini before
// 4.2.4) ignores the file, so IP allowlists and the admin console's
// networks are stored but not enforced. It returns why not, or "".
func (a *Admin) EnforcesHBA(ctx context.Context) (string, error) {
	v, err := a.Setting(ctx, "auth_type")
	if err != nil {
		return "", err
	}
	if v != "hba" {
		return fmt.Sprintf("PgBouncer %s has auth_type = %s, not hba: it ignores hba.conf, so IP allowlists are not enforced (update its pgbouncer.ini)", a.Name, v), nil
	}
	return "", nil
}

// Ping checks the admin console answers (SHOW VERSION).
func (a *Admin) Ping(ctx context.Context) error { return a.exec(ctx, "SHOW VERSION") }

// KillUser drops every client connection logged in as user (KILL_CLIENT,
// PgBouncer 1.24+), leaving other users of the same database alone. It
// returns how many clients it dropped.
func (a *Admin) KillUser(ctx context.Context, user string) (int, error) {
	if !dbNameRe.MatchString(user) {
		return 0, fmt.Errorf("pooler: invalid user name %q", user)
	}
	return a.KillClients(ctx, func(c Client) bool { return c.User == user })
}

// Client is one row of SHOW CLIENTS.
type Client struct {
	ID, User, Database string
	// Addr is the client's address, or "unix" for a socket.
	Addr string
}

// KillClients drops every client connection drop selects (KILL_CLIENT,
// PgBouncer 1.24+) and returns how many it dropped.
func (a *Admin) KillClients(ctx context.Context, drop func(Client) bool) (int, error) {
	conn, err := pgx.ConnectConfig(ctx, a.cfg)
	if err != nil {
		return 0, fmt.Errorf("pooler %s (%s): connect admin console: %w", a.Name, a.Addr(), err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, "SHOW CLIENTS")
	if err != nil {
		return 0, fmt.Errorf("pooler %s: SHOW CLIENTS: %w", a.Name, err)
	}
	col := map[string]int{}
	for i, fd := range rows.FieldDescriptions() {
		col[fd.Name] = i
	}
	field := func(raw [][]byte, name string) string {
		if i, ok := col[name]; ok && i < len(raw) {
			return string(raw[i])
		}
		return ""
	}
	var ids []string
	for rows.Next() {
		raw := rows.RawValues()
		c := Client{ID: field(raw, "id"), User: field(raw, "user"), Database: field(raw, "database"), Addr: field(raw, "addr")}
		if c.ID != "" && c.User != "" && drop(c) {
			ids = append(ids, c.ID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if _, err := strconv.ParseUint(id, 10, 64); err != nil {
			continue
		}
		if _, err := conn.Exec(ctx, "KILL_CLIENT "+id); err != nil {
			return n, fmt.Errorf("pooler %s: KILL_CLIENT %s: %w", a.Name, id, err)
		}
		n++
	}
	return n, nil
}
