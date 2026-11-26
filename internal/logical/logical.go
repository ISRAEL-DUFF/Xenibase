// Package logical moves one database between Postgres servers with
// logical replication (V3 §2.3): a publication on the source, a
// subscription on the target that copies the data and then streams
// changes, and a cutover that only has to wait for the last few commits.
//
// It works on connections only. Callers (internal/dedicated) create the
// target database and its schema, freeze writes around Cutover, and switch
// the route; this package does the replication itself.
package logical

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Prefix names every object a move creates on either side.
const Prefix = "xenibase_move_"

// Schema holds the move's own objects on both sides: the DDL block's
// function and the marker table the cutover syncs on. Superuser-owned,
// dropped by Cleanup, and never part of a dump.
const Schema = "xenibase_move"

const schema = Schema

// Margin is added to each sequence on the target at cutover. Writes are
// frozen before sequences are read, so it only guards against values taken
// by a transaction that never committed.
const Margin = 1000

// Name returns the publication, subscription, slot and login name for a
// move identified by id (an operation or instance ID).
func Name(id string) string {
	id = strings.ToLower(strings.ReplaceAll(id, "-", ""))
	if len(id) > 16 {
		id = id[:16]
	}
	return Prefix + id
}

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Issue is one preflight finding.
type Issue struct {
	Check  string `json:"check"`
	Detail string `json:"detail"`
}

// Report is a preflight: Blockers make a logical move impossible (the
// caller falls back to dump/restore or refuses); NoIdentity tables get
// REPLICA IDENTITY FULL for the move.
type Report struct {
	SourceVersion int      `json:"source_version"`
	TargetVersion int      `json:"target_version"`
	SizeBytes     int64    `json:"size_bytes"`
	Blockers      []Issue  `json:"blockers,omitempty"`
	NoIdentity    []string `json:"no_identity,omitempty"`
	Extensions    []string `json:"extensions,omitempty"`
}

// OK reports whether a logical move can run.
func (r Report) OK() bool { return len(r.Blockers) == 0 }

// Reason summarises the blockers.
func (r Report) Reason() string {
	var parts []string
	for _, b := range r.Blockers {
		parts = append(parts, b.Detail)
	}
	return strings.Join(parts, "; ")
}

// Preflight checks src (the database being moved, as the superuser) and
// target (any database on the target server, as the superuser). A nil
// target checks the source alone (an estimate before a target is chosen).
func Preflight(ctx context.Context, src, target *pgx.Conn) (Report, error) {
	var r Report
	var walLevel string
	var slotsFree, sendersFree int
	err := src.QueryRow(ctx, `SELECT current_setting('server_version_num')::int, current_setting('wal_level'),
		  current_setting('max_replication_slots')::int - (SELECT count(*) FROM pg_replication_slots),
		  current_setting('max_wal_senders')::int - (SELECT count(*) FROM pg_stat_replication),
		  pg_database_size(current_database())`).Scan(&r.SourceVersion, &walLevel, &slotsFree, &sendersFree, &r.SizeBytes)
	if err != nil {
		return r, fmt.Errorf("preflight source: %w", err)
	}
	r.TargetVersion = r.SourceVersion
	if target != nil {
		if err := target.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&r.TargetVersion); err != nil {
			return r, fmt.Errorf("preflight target: %w", err)
		}
	}
	block := func(check, format string, a ...any) {
		r.Blockers = append(r.Blockers, Issue{Check: check, Detail: fmt.Sprintf(format, a...)})
	}
	if walLevel != "logical" {
		block("wal_level", "the source runs wal_level=%s; logical replication needs wal_level=logical (set it and restart the server)", walLevel)
	}
	if slotsFree < 1 {
		block("replication_slots", "the source has no free replication slot (max_replication_slots)")
	}
	if sendersFree < 1 {
		block("wal_senders", "the source has no free WAL sender (max_wal_senders)")
	}
	if r.TargetVersion/10000 < r.SourceVersion/10000 {
		block("version", "the target runs Postgres %d, older than the source's %d", r.TargetVersion/10000, r.SourceVersion/10000)
	}

	var largeObjects bool
	if err := src.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_largeobject_metadata)`).Scan(&largeObjects); err != nil {
		return r, err
	}
	if largeObjects {
		block("large_objects", "the database has large objects (lo_*), which logical replication doesn't copy")
	}
	unlogged, err := names(ctx, src, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND c.relpersistence = 'u' AND `+userSchema+` ORDER BY 1`)
	if err != nil {
		return r, err
	}
	if len(unlogged) > 0 {
		block("unlogged_tables", "unlogged tables aren't replicated: %s", list(unlogged))
	}

	// Tables UPDATE and DELETE can't be replicated for as they are.
	r.NoIdentity, err = names(ctx, src, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND c.relpersistence = 'p' AND `+userSchema+`
		  AND (c.relreplident = 'n' OR (c.relreplident = 'd' AND NOT EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary)))
		ORDER BY 1`)
	if err != nil {
		return r, err
	}

	r.Extensions, err = names(ctx, src, `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql' ORDER BY 1`)
	if err != nil {
		return r, err
	}
	if len(r.Extensions) > 0 && target != nil {
		avail, err := names(ctx, target, `SELECT name FROM pg_available_extensions`)
		if err != nil {
			return r, err
		}
		var missing []string
		for _, e := range r.Extensions {
			if !slices.Contains(avail, e) {
				missing = append(missing, e)
			}
		}
		if len(missing) > 0 {
			block("extensions", "the target can't create extension(s) %s", list(missing))
		}
	}
	return r, nil
}

// userSchema matches a relation's namespace (alias n) that is the
// tenant's, not the system's or the move's.
const userSchema = `n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast', '` + schema + `') AND n.nspname NOT LIKE 'pg_temp%' AND n.nspname NOT LIKE 'pg_toast_temp%'`

func names(ctx context.Context, c *pgx.Conn, q string, args ...any) ([]string, error) {
	rows, err := c.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func list(xs []string) string {
	if len(xs) > 10 {
		return strings.Join(xs[:10], ", ") + fmt.Sprintf(" and %d more", len(xs)-10)
	}
	return strings.Join(xs, ", ")
}

// Endpoint is how the target server reaches the source, as the server
// (not the control plane) sees it.
type Endpoint struct {
	Host, SSLMode string
	Port          int
}

// Move is one database's replication.
type Move struct {
	Name     string // publication, subscription, slot and login
	Database string
	// Same major version: the initial copy can use the binary format.
	SameMajor bool
}

// Setup prepares the source and target and starts replication: replica
// identities, the DDL block, the marker table, the replication login, the
// publication, and the target's subscription. srcDB is the database being
// moved and srcAdmin the source server's postgres database (roles are
// cluster-wide), both as the superuser; dstDB is the target database,
// already holding the schema, as the superuser.
func Setup(ctx context.Context, m Move, srcDB, srcAdmin, dstDB *pgx.Conn, at Endpoint, noIdentity []string) error {
	for _, t := range noIdentity {
		if _, err := srcDB.Exec(ctx, "ALTER TABLE "+t+" REPLICA IDENTITY FULL"); err != nil {
			return fmt.Errorf("replica identity for %s: %w", t, err)
		}
	}
	// The move's schema on both sides: the marker table must exist on the
	// target for the subscription to apply into it.
	for _, c := range []*pgx.Conn{srcDB, dstDB} {
		if _, err := c.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+schema+`;
			REVOKE ALL ON SCHEMA `+schema+` FROM PUBLIC;
			CREATE TABLE IF NOT EXISTS `+schema+`.marker (token text PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return fmt.Errorf("move schema: %w", err)
		}
	}
	if err := blockDDL(ctx, srcDB); err != nil {
		return err
	}

	password, err := secret()
	if err != nil {
		return err
	}
	var exists bool
	if err := srcAdmin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, m.Name).Scan(&exists); err != nil {
		return err
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	if _, err := srcAdmin.Exec(ctx, verb+" ROLE "+ident(m.Name)+" LOGIN REPLICATION BYPASSRLS NOINHERIT PASSWORD "+quote(password)); err != nil {
		return fmt.Errorf("replication login: %w", err)
	}
	if _, err := srcAdmin.Exec(ctx, "GRANT CONNECT ON DATABASE "+ident(m.Database)+" TO "+ident(m.Name)); err != nil {
		return err
	}
	schemas, err := names(ctx, srcDB, `SELECT nspname FROM pg_namespace n WHERE `+userSchema+` OR n.nspname = '`+schema+`'`)
	if err != nil {
		return err
	}
	for _, s := range schemas {
		if _, err := srcDB.Exec(ctx, "GRANT USAGE ON SCHEMA "+ident(s)+" TO "+ident(m.Name)+"; GRANT SELECT ON ALL TABLES IN SCHEMA "+ident(s)+" TO "+ident(m.Name)); err != nil {
			return fmt.Errorf("grant %s: %w", s, err)
		}
	}
	if err := srcDB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = $1)`, m.Name).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := srcDB.Exec(ctx, "CREATE PUBLICATION "+ident(m.Name)+" FOR ALL TABLES"); err != nil {
			return fmt.Errorf("publication: %w", err)
		}
	}

	conninfo := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s application_name=%s",
		connValue(at.Host), at.Port, connValue(m.Database), connValue(m.Name), connValue(password), connValue(orDefault(at.SSLMode, "prefer")), m.Name)
	if err := dstDB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_subscription WHERE subname = $1)`, m.Name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		// A retry: the login's password changed.
		_, err := dstDB.Exec(ctx, "ALTER SUBSCRIPTION "+ident(m.Name)+" CONNECTION "+quote(conninfo))
		return err
	}
	opts := "copy_data = true, create_slot = true, slot_name = " + quote(m.Name) + ", disable_on_error = true"
	if m.SameMajor {
		opts += ", binary = true"
	}
	if _, err := dstDB.Exec(ctx, "CREATE SUBSCRIPTION "+ident(m.Name)+" CONNECTION "+quote(conninfo)+" PUBLICATION "+ident(m.Name)+" WITH ("+opts+")"); err != nil {
		return fmt.Errorf("subscription: %w", err)
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// connValue quotes a libpq connection-string value.
func connValue(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func secret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// blockDDL installs the event trigger that refuses tenants' DDL while the
// move runs (V3 §2.3): the target's schema was copied once and DDL isn't
// replicated. The superuser (Xenibase itself) is let through.
func blockDDL(ctx context.Context, c *pgx.Conn) error {
	_, err := c.Exec(ctx, `
CREATE OR REPLACE FUNCTION `+schema+`.block_ddl() RETURNS event_trigger LANGUAGE plpgsql AS $f$
BEGIN
  IF NOT (SELECT rolsuper FROM pg_roles WHERE rolname = session_user) THEN
    RAISE EXCEPTION 'this project is being moved; schema changes are paused until the move finishes'
      USING ERRCODE = 'object_not_in_prerequisite_state',
            HINT = 'Try again in a few minutes. Data changes keep working.';
  END IF;
END
$f$;
DROP EVENT TRIGGER IF EXISTS `+Prefix+`ddl_block;
CREATE EVENT TRIGGER `+Prefix+`ddl_block ON ddl_command_start EXECUTE FUNCTION `+schema+`.block_ddl()`)
	if err != nil {
		return fmt.Errorf("DDL block: %w", err)
	}
	return nil
}

// Status is replication progress.
type Status struct {
	Tables, Ready int
	// LagBytes is the source's WAL not yet confirmed by the target.
	LagBytes int64
	Enabled  bool
	// Errors counts apply and sync errors the target recorded.
	Errors int64
}

// Caught reports whether every table finished its initial copy.
func (s Status) Caught() bool { return s.Tables > 0 && s.Ready == s.Tables }

// Progress reads replication progress from both sides.
func Progress(ctx context.Context, m Move, srcDB, dstDB *pgx.Conn) (Status, error) {
	var st Status
	err := dstDB.QueryRow(ctx, `SELECT s.subenabled,
		  (SELECT count(*) FROM pg_subscription_rel r WHERE r.srsubid = s.oid),
		  (SELECT count(*) FROM pg_subscription_rel r WHERE r.srsubid = s.oid AND r.srsubstate = 'r'),
		  COALESCE((SELECT apply_error_count + sync_error_count FROM pg_stat_subscription_stats WHERE subid = s.oid), 0)
		FROM pg_subscription s WHERE s.subname = $1`, m.Name).Scan(&st.Enabled, &st.Tables, &st.Ready, &st.Errors)
	if err != nil {
		return st, fmt.Errorf("subscription progress: %w", err)
	}
	var lag *int64
	if err := srcDB.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)::bigint FROM pg_replication_slots WHERE slot_name = $1`,
		m.Name).Scan(&lag); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return st, fmt.Errorf("slot lag: %w", err)
	}
	if lag != nil {
		st.LagBytes = *lag
	}
	return st, nil
}

// ErrReplication means the target stopped applying (disable_on_error).
var ErrReplication = errors.New("replication stopped on the target")

// lastError is the subscription's most recent error from the server log
// view, when Postgres exposes it (it doesn't; the count is all there is).
func replicationError(st Status) error {
	return fmt.Errorf("%w (%d apply or copy error(s); see the target server's log)", ErrReplication, st.Errors)
}

// WaitOptions tune WaitCaughtUp.
type WaitOptions struct {
	// MaxLag and StableFor: lag must stay under MaxLag for StableFor
	// (defaults 1 MB and 30 s, V3 §2.3 step 4).
	MaxLag    int64
	StableFor time.Duration
	// Poll interval (default 2 s).
	Poll time.Duration
	// Report is called with each reading; returning an error stops.
	Report func(Status) error
}

// WaitCaughtUp waits until every table is copied and the lag has stayed
// low, or ctx ends.
func WaitCaughtUp(ctx context.Context, m Move, srcDB, dstDB *pgx.Conn, o WaitOptions) error {
	if o.MaxLag == 0 {
		o.MaxLag = 1 << 20
	}
	if o.StableFor == 0 {
		o.StableFor = 30 * time.Second
	}
	if o.Poll == 0 {
		o.Poll = 2 * time.Second
	}
	var lowSince time.Time
	for {
		st, err := Progress(ctx, m, srcDB, dstDB)
		if err != nil {
			return err
		}
		if !st.Enabled {
			return replicationError(st)
		}
		if o.Report != nil {
			if err := o.Report(st); err != nil {
				return err
			}
		}
		switch {
		case !st.Caught() || st.LagBytes > o.MaxLag:
			lowSince = time.Time{}
		case lowSince.IsZero():
			lowSince = time.Now()
		case time.Since(lowSince) >= o.StableFor:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.Poll):
		}
	}
}

// Result is what a cutover did.
type Result struct {
	// SyncWait is how long the last commits took to arrive.
	SyncWait  time.Duration
	Sequences int
	Verified  []TableCount
}

// TableCount is one sampled table's rows on both sides.
type TableCount struct {
	Table    string `json:"table"`
	Source   int64  `json:"source"`
	Target   int64  `json:"target"`
	Matching bool   `json:"matching"`
}

// ErrMismatch means a sampled table's rows differ after the sync.
var ErrMismatch = errors.New("row counts differ between source and target")

// Cutover runs while writes to the source are frozen (V3 §2.3 step 5): it
// waits until the target has applied everything the source committed,
// copies the sequences with a margin, and compares row counts on a sample
// of tables. The caller then switches the route.
func Cutover(ctx context.Context, m Move, srcDB, dstDB *pgx.Conn, timeout time.Duration) (Result, error) {
	var res Result
	start := time.Now()
	// A marker committed after the freeze: once the target has it, it has
	// every earlier commit (changes are applied in commit order).
	token, err := secret()
	if err != nil {
		return res, err
	}
	if _, err := srcDB.Exec(ctx, `INSERT INTO `+schema+`.marker (token) VALUES ($1)`, token); err != nil {
		return res, fmt.Errorf("sync marker: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		var got bool
		if err := dstDB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+schema+`.marker WHERE token = $1)`, token).Scan(&got); err != nil {
			return res, err
		}
		if got {
			break
		}
		if time.Now().After(deadline) {
			st, _ := Progress(ctx, m, srcDB, dstDB)
			if !st.Enabled {
				return res, replicationError(st)
			}
			return res, fmt.Errorf("the target didn't catch up within %s (lag %d bytes)", timeout, st.LagBytes)
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	res.SyncWait = time.Since(start)

	n, err := CopySequences(ctx, srcDB, dstDB, Margin)
	if err != nil {
		return res, err
	}
	res.Sequences = n

	res.Verified, err = SampleCounts(ctx, srcDB, dstDB, 20)
	if err != nil {
		return res, err
	}
	var bad []string
	for _, t := range res.Verified {
		if !t.Matching {
			bad = append(bad, fmt.Sprintf("%s (%d on the source, %d on the target)", t.Table, t.Source, t.Target))
		}
	}
	if len(bad) > 0 {
		return res, fmt.Errorf("%w: %s", ErrMismatch, strings.Join(bad, ", "))
	}
	return res, nil
}

// CopySequences sets each of the tenant's sequences on dst to its value on
// src plus margin increments (V3 §2.3: logical replication doesn't carry
// sequences). Sequences never used on src are left alone.
func CopySequences(ctx context.Context, src, dst *pgx.Conn, margin int64) (int, error) {
	rows, err := src.Query(ctx, `SELECT format('%I.%I', schemaname, sequencename), last_value, increment_by, min_value, max_value
		FROM pg_sequences WHERE last_value IS NOT NULL AND schemaname NOT IN ('pg_catalog', 'information_schema', '`+schema+`')`)
	if err != nil {
		return 0, err
	}
	type seq struct {
		name               string
		last, step, lo, hi int64
	}
	seqs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (seq, error) {
		var s seq
		return s, r.Scan(&s.name, &s.last, &s.step, &s.lo, &s.hi)
	})
	if err != nil {
		return 0, err
	}
	for _, s := range seqs {
		v := next(s.last, s.step, margin, s.lo, s.hi)
		if _, err := dst.Exec(ctx, "SELECT setval($1::regclass, $2, true)", s.name, v); err != nil {
			return 0, fmt.Errorf("sequence %s: %w", s.name, err)
		}
	}
	return len(seqs), nil
}

// next is last advanced by margin steps, kept within [lo, hi].
func next(last, step, margin, lo, hi int64) int64 {
	add := step * margin
	switch {
	case add > 0 && last > hi-add:
		return hi
	case add < 0 && last < lo-add:
		return lo
	}
	return last + add
}

// sampleMaxBytes bounds the tables SampleCounts counts, so the check costs
// milliseconds while writes are frozen whatever the database's size.
const sampleMaxBytes = 16 << 20

// SampleCounts compares exact row counts of up to n of the tenant's
// smallest tables on disk (at most sampleMaxBytes each), the ones cheap to
// count while writes are frozen. The marker already shows the target has
// every change; this is a second, independent check.
func SampleCounts(ctx context.Context, src, dst *pgx.Conn, n int) ([]TableCount, error) {
	tables, err := names(ctx, src, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND c.relpersistence = 'p' AND `+userSchema+` AND pg_total_relation_size(c.oid) <= $2
		ORDER BY pg_total_relation_size(c.oid), 1 LIMIT $1`, n, sampleMaxBytes)
	if err != nil {
		return nil, err
	}
	out := make([]TableCount, 0, len(tables))
	for _, t := range tables {
		tc := TableCount{Table: t}
		if err := src.QueryRow(ctx, "SELECT count(*) FROM "+t).Scan(&tc.Source); err != nil {
			return nil, fmt.Errorf("count %s on the source: %w", t, err)
		}
		if err := dst.QueryRow(ctx, "SELECT count(*) FROM "+t).Scan(&tc.Target); err != nil {
			return nil, fmt.Errorf("count %s on the target: %w", t, err)
		}
		tc.Matching = tc.Source == tc.Target
		out = append(out, tc)
	}
	return out, nil
}

// Cleanup removes everything Setup created, on whichever side is
// reachable, and is safe to repeat: the subscription without touching the
// source (its slot is dropped directly), the slot, publication, DDL block,
// marker and login. dst may be nil when the target is gone.
func Cleanup(ctx context.Context, m Move, srcDB, srcAdmin, dstDB *pgx.Conn) error {
	var errs []error
	if dstDB != nil {
		var exists bool
		if err := dstDB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_subscription WHERE subname = $1)`, m.Name).Scan(&exists); err != nil {
			errs = append(errs, err)
		} else if exists {
			if err := DropSubscription(ctx, dstDB, m.Name); err != nil {
				errs = append(errs, err)
			}
		}
		if _, err := dstDB.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			errs = append(errs, err)
		}
	}
	if srcDB != nil {
		if err := dropSlot(ctx, srcDB, m.Name); err != nil {
			errs = append(errs, err)
		}
		for _, stmt := range []string{
			"DROP PUBLICATION IF EXISTS " + ident(m.Name),
			"DROP EVENT TRIGGER IF EXISTS " + Prefix + "ddl_block",
			"DROP SCHEMA IF EXISTS " + schema + " CASCADE",
		} {
			if _, err := srcDB.Exec(ctx, stmt); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", stmt, err))
			}
		}
		var exists bool
		if err := srcAdmin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, m.Name).Scan(&exists); err != nil {
			errs = append(errs, err)
		} else if exists {
			// Its grants live in the moved database.
			if _, err := srcDB.Exec(ctx, "DROP OWNED BY "+ident(m.Name)); err != nil {
				errs = append(errs, err)
			} else if _, err := srcAdmin.Exec(ctx, "DROP ROLE "+ident(m.Name)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Leftovers are the move objects in one database: what a cleanup that
// could not reach a side left there.
type Leftovers struct {
	Subscriptions, Publications []string
	// Slots are the inactive slots on the database.
	Slots []string
	// Logins are the move logins granted CONNECT on the database.
	Logins []string
	// DDLBlock: the event trigger or the move schema is there.
	DDLBlock bool
}

// FindLeftovers lists the move objects in the database c is connected to,
// as the superuser.
func FindLeftovers(ctx context.Context, c *pgx.Conn) (Leftovers, error) {
	var lo Leftovers
	var err error
	if lo.Subscriptions, err = names(ctx, c, `SELECT subname FROM pg_subscription
		WHERE subdbid = (SELECT oid FROM pg_database WHERE datname = current_database()) AND starts_with(subname::text, $1) ORDER BY 1`, Prefix); err != nil {
		return lo, err
	}
	if lo.Publications, err = names(ctx, c, `SELECT pubname FROM pg_publication WHERE starts_with(pubname::text, $1) ORDER BY 1`, Prefix); err != nil {
		return lo, err
	}
	if lo.Slots, err = names(ctx, c, `SELECT slot_name FROM pg_replication_slots
		WHERE database = current_database() AND NOT active AND starts_with(slot_name::text, $1) ORDER BY 1`, Prefix); err != nil {
		return lo, err
	}
	if lo.Logins, err = names(ctx, c, `SELECT DISTINCT r.rolname FROM pg_database d CROSS JOIN LATERAL aclexplode(d.datacl) a
		JOIN pg_roles r ON r.oid = a.grantee WHERE d.datname = current_database() AND starts_with(r.rolname::text, $1) ORDER BY 1`, Prefix); err != nil {
		return lo, err
	}
	err = c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_event_trigger WHERE evtname = $1) OR EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $2)`,
		Prefix+"ddl_block", schema).Scan(&lo.DDLBlock)
	return lo, err
}

// DropSubscription drops a subscription in c's database without touching
// its publisher (its slot is dropped there separately).
func DropSubscription(ctx context.Context, c *pgx.Conn, name string) error {
	for _, stmt := range []string{
		"ALTER SUBSCRIPTION " + ident(name) + " DISABLE",
		"ALTER SUBSCRIPTION " + ident(name) + " SET (slot_name = NONE)",
		"DROP SUBSCRIPTION " + ident(name),
	} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

// DropPublication drops a publication in c's database.
func DropPublication(ctx context.Context, c *pgx.Conn, name string) error {
	_, err := c.Exec(ctx, "DROP PUBLICATION IF EXISTS "+ident(name))
	return err
}

// DropInactiveSlot drops a slot no WAL sender holds.
func DropInactiveSlot(ctx context.Context, c *pgx.Conn, name string) error {
	_, err := c.Exec(ctx, `SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = $1 AND NOT active`, name)
	return err
}

// DropDDLBlock removes the event trigger and the move schema from c's
// database.
func DropDDLBlock(ctx context.Context, c *pgx.Conn) error {
	for _, stmt := range []string{"DROP EVENT TRIGGER IF EXISTS " + Prefix + "ddl_block", "DROP SCHEMA IF EXISTS " + schema + " CASCADE"} {
		if _, err := c.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}

// DropLoginGrants revokes what a move login holds in c's database (and
// its CONNECT); the login itself is dropped from the server once no
// database has any.
func DropLoginGrants(ctx context.Context, c *pgx.Conn, name string) error {
	_, err := c.Exec(ctx, "DROP OWNED BY "+ident(name))
	return err
}

// DropLogin drops a move login from the server.
func DropLogin(ctx context.Context, admin *pgx.Conn, name string) error {
	_, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+ident(name))
	return err
}

// dropSlot drops the slot, ending the WAL sender still holding it.
func dropSlot(ctx context.Context, c *pgx.Conn, slot string) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		var pid *int32
		err := c.QueryRow(ctx, `SELECT active_pid FROM pg_replication_slots WHERE slot_name = $1`, slot).Scan(&pid)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if pid != nil {
			_, _ = c.Exec(ctx, `SELECT pg_terminate_backend($1)`, *pid)
		} else if _, err := c.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slot); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("drop replication slot %s: %w", slot, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("replication slot %s is still in use", slot)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
