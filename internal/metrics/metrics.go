// Package metrics samples project and node metrics into metric_points,
// downsamples them, and serves series and the Prometheus exposition
// (spec §8.7).
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Scopes of metric_points.
const (
	ScopeProject = "project"
	ScopeNode    = "node"
	// ScopeInstance is a Postgres server, a shared cluster or a dedicated
	// instance; the webhook deliverer writes its points.
	ScopeInstance = "instance"
)

// Project metrics.
const (
	SizeBytes         = "size_bytes"
	ConnectionsActive = "connections_active"
	ConnectionsIdle   = "connections_idle"
	PoolerClients     = "pooler_clients"
	PoolerWaiting     = "pooler_waiting"
	TPS               = "tps"
	CacheHitRatio     = "cache_hit_ratio"
	// A dedicated instance's WAL archiving: failed attempts (a counter),
	// seconds since the last segment was archived, segments waiting, and
	// seconds the oldest has waited.
	WALArchiveFailed     = "wal_archive_failed"
	WALArchiveAge        = "wal_archive_age_s"
	WALArchivePending    = "wal_archive_pending"
	WALArchivePendingAge = "wal_archive_pending_age_s"
)

// Node metrics.
const (
	CPUPercent     = "cpu_percent"
	Load1          = "load1"
	MemUsedBytes   = "mem_used_bytes"
	MemTotalBytes  = "mem_total_bytes"
	DiskUsedBytes  = "disk_used_bytes"
	DiskTotalBytes = "disk_total_bytes"
	DiskReadBPS    = "disk_read_bps"
	DiskWriteBPS   = "disk_write_bps"
)

// Instance metrics.
const (
	// WebhookListenConnections are the direct connections the webhook
	// deliverer holds LISTENing on an instance's databases, outside the
	// poolers (XENIBASE_WEBHOOK_LISTEN_MAX caps them).
	WebhookListenConnections = "webhook_listen_connections"
)

// sizeEvery is how often database sizes are sampled (spec §8.7: 5 min);
// pg_database_size walks the whole directory.
const sizeEvery = 5 * time.Minute

// lockKey makes one xenibase-server at a time the collector.
const lockKey int64 = 0x7067646f636b0a // "xenibase\x0a"; every key must be distinct (TestAdvisoryLockKeysDistinct)

// Projects lists projects and configures connections to their instances
// (*provision.Service).
type Projects interface {
	List(ctx context.Context, status *string, limit int) ([]store.Project, error)
	AdminConfig(ctx context.Context, instanceID uuid.UUID, database string) (*pgx.ConnConfig, error)
}

// Poolers are the PgBouncer admin consoles (*pooler.Manager).
type Poolers interface {
	LocalAdmins() []*pooler.Admin
	HostAdmins() []*pooler.Admin
}

// Collector samples metrics on an interval.
//
// One xenibase-server at a time collects, under a session advisory lock on
// a connection of its own: it lists the projects, samples every instance
// over a connection it keeps between rounds, reads the pooler hosts, writes
// metric_points and downsamples. Every server reads the PgBouncers next to
// it, which only it administers, and leaves the readings in
// pooler_readings; the collecting server adds them up into pooler_clients,
// pooler_waiting and the Free tier's activity. /metrics reads
// metric_points, so every server serves it.
type Collector struct {
	db       *pgxpool.Pool
	projects Projects
	pooler   Poolers
	nodes    *nodes.Service
	interval time.Duration
	log      *slog.Logger
	// source names this process's pooler readings.
	source string

	lockMu   sync.Mutex
	lockConn *pgx.Conn // holds lockKey while this server collects

	connMu sync.Mutex
	conns  map[uuid.UUID]*instanceConn

	mu     sync.Mutex
	prevDB map[uuid.UUID]dbCounters
	// prevXacts are the poolers' transaction counters per database at the
	// last sample, keyed by reading source and database (activity, V3 §4.2).
	prevXacts map[string]xactCount
	prevNode  map[uuid.UUID]nodeCounters
	lastSize  time.Time
}

type xactCount struct {
	at time.Time
	n  int64
}

// instanceConn is the collector's admin connection to one instance, kept
// between rounds. It is closed when idle, when the instance's address or
// credentials change, when no active project is left on the instance, and
// when this server stops collecting.
type instanceConn struct {
	mu   sync.Mutex
	key  string // address and credentials it was opened with
	conn *pgx.Conn
	used time.Time
}

type dbCounters struct {
	at                 time.Time
	xacts, hits, reads int64
}

type nodeCounters struct {
	at                      time.Time
	busy, total, read, writ uint64
}

// NewCollector returns a Collector. pm and ns may be nil (no poolers, no
// agents).
func NewCollector(db *pgxpool.Pool, projects Projects, pm *pooler.Manager, ns *nodes.Service, interval time.Duration, log *slog.Logger) *Collector {
	if interval <= 0 {
		interval = time.Minute
	}
	c := &Collector{
		db: db, projects: projects, nodes: ns, interval: interval, log: log, source: uuid.NewString(),
		prevDB: map[uuid.UUID]dbCounters{}, prevNode: map[uuid.UUID]nodeCounters{}, prevXacts: map[string]xactCount{},
		conns: map[uuid.UUID]*instanceConn{},
	}
	if pm != nil {
		c.pooler = pm
	}
	return c
}

// Interval is the sampling interval.
func (c *Collector) Interval() time.Duration { return c.interval }

// Run samples every interval and downsamples every hour until ctx ends,
// while this server holds the collector lock. Without it, a round only
// reads this server's own poolers.
func (c *Collector) Run(ctx context.Context) {
	defer c.Close()
	t := time.NewTicker(c.interval)
	defer t.Stop()
	// After a restart, or on taking over from another server, catch up on
	// every hour 1-minute points still cover.
	var nextDownsample, since time.Time
	for {
		led, err := c.Round(ctx)
		if err != nil && ctx.Err() == nil {
			c.log.Warn("collect metrics", "err", err)
		}
		switch {
		case !led:
			nextDownsample = time.Time{}
		case nextDownsample.IsZero() || time.Now().After(nextDownsample):
			if nextDownsample.IsZero() {
				since = time.Now().Add(-25 * time.Hour)
			}
			if err := c.Downsample(ctx, since); err != nil && ctx.Err() == nil {
				c.log.Warn("downsample metrics", "err", err)
			}
			nextDownsample, since = time.Now().Add(time.Hour), time.Now().Add(-3*time.Hour)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Round is one tick of Run: this server's pooler readings, then, if it
// holds or takes the collector lock, a sample of everything. led says
// whether this server collected.
func (c *Collector) Round(ctx context.Context) (led bool, err error) {
	var errs []error
	if err := c.publishLocal(ctx); err != nil {
		errs = append(errs, err)
	}
	led, err = c.lead(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	if !led {
		// Instance connections are the collecting server's alone.
		c.closeConns(func(uuid.UUID, *instanceConn) bool { return true })
		return false, errors.Join(errs...)
	}
	if err := c.collect(ctx); err != nil {
		errs = append(errs, err)
	}
	return true, errors.Join(errs...)
}

// lead reports whether this server holds the collector lock, taking it if
// it is free. The lock is a session lock on a connection of its own, kept
// while this server collects, so no pooled connection is ever handed back
// still holding it, and the lock ends with the connection if the server
// dies.
func (c *Collector) lead(ctx context.Context) (bool, error) {
	c.lockMu.Lock()
	defer c.lockMu.Unlock()
	if c.lockConn != nil {
		if err := c.lockConn.Ping(ctx); err == nil {
			return true, nil
		}
		_ = c.lockConn.Close(context.Background())
		c.lockConn = nil
	}
	conn, err := pgx.ConnectConfig(ctx, c.db.Config().ConnConfig.Copy())
	if err != nil {
		return false, fmt.Errorf("collector lock: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&got); err != nil || !got {
		_ = conn.Close(context.Background())
		if err != nil {
			return false, fmt.Errorf("collector lock: %w", err)
		}
		return false, nil // another server collects
	}
	c.lockConn = conn
	return true, nil
}

// Close lets go of the collector lock and the instance connections.
func (c *Collector) Close() {
	c.lockMu.Lock()
	if c.lockConn != nil {
		_ = c.lockConn.Close(context.Background()) // ends the session lock
		c.lockConn = nil
	}
	c.lockMu.Unlock()
	c.closeConns(func(uuid.UUID, *instanceConn) bool { return true })
}

// Downsample averages completed hours since since into 1-hour points and
// prunes points past retention (1m for 24 hours, 1h for 30 days).
func (c *Collector) Downsample(ctx context.Context, since time.Time) error {
	q := store.New(c.db)
	n, err := q.DownsampleMetrics(ctx, since)
	if err != nil {
		return err
	}
	pruned, err := q.PruneMetrics(ctx)
	if err != nil {
		return err
	}
	c.log.Debug("metrics downsampled", "hour_points", n, "pruned", pruned)
	return nil
}

// batch accumulates points for one timestamp.
type batch struct {
	ids  []uuid.UUID
	mets []string
	vals []float64
}

func (b *batch) add(id uuid.UUID, metric string, v float64) {
	b.ids, b.mets, b.vals = append(b.ids, id), append(b.mets, metric), append(b.vals, v)
}

func (b *batch) addAll(o batch) {
	b.ids, b.mets, b.vals = append(b.ids, o.ids...), append(b.mets, o.mets...), append(b.vals, o.vals...)
}

// bucket is the 1-minute point a sample at t belongs to.
func bucket(t time.Time) time.Time { return t.UTC().Truncate(time.Minute) }

// CollectSizes is Collect with database sizes measured now, however recently
// they were last (tests, and storage enforcement right after Reclaim space).
func (c *Collector) CollectSizes(ctx context.Context) error {
	c.mu.Lock()
	c.lastSize = time.Time{}
	c.mu.Unlock()
	return c.Collect(ctx)
}

// Collect takes one sample of every active project and every node now,
// whether or not this server holds the collector lock (tests, and callers
// that want a fresh point at once).
func (c *Collector) Collect(ctx context.Context) error {
	err := c.publishLocal(ctx)
	return errors.Join(err, c.collect(ctx))
}

func (c *Collector) collect(ctx context.Context) error {
	now := time.Now()
	ts := bucket(now)
	var errs []error
	var proj batch
	if err := c.collectProjects(ctx, now, &proj); err != nil {
		errs = append(errs, err)
	}
	q := store.New(c.db)
	if len(proj.ids) > 0 {
		if err := q.UpsertMetricPoints(ctx, store.UpsertMetricPointsParams{
			Scope: ScopeProject, ScopeIds: proj.ids, Metrics: proj.mets, Ts: ts, Vals: proj.vals,
		}); err != nil {
			errs = append(errs, err)
		}
	}
	var node batch
	if err := c.collectNodes(ctx, now, &node); err != nil {
		errs = append(errs, err)
	}
	if len(node.ids) > 0 {
		if err := q.UpsertMetricPoints(ctx, store.UpsertMetricPointsParams{
			Scope: ScopeNode, ScopeIds: node.ids, Metrics: node.mets, Ts: ts, Vals: node.vals,
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Collector) collectProjects(ctx context.Context, now time.Time, b *batch) error {
	active := provision.StatusActive
	ps, err := c.projects.List(ctx, &active, 100000)
	if err != nil {
		return err
	}
	byInstance := map[uuid.UUID][]store.Project{}
	for _, p := range ps {
		byInstance[p.InstanceID] = append(byInstance[p.InstanceID], p)
	}

	c.mu.Lock()
	withSize := c.lastSize.IsZero() || now.Sub(c.lastSize) >= min(sizeEvery, 5*c.interval)
	if withSize {
		c.lastSize = now
	}
	c.mu.Unlock()

	var errs []error
	for instID, group := range byInstance {
		if err := c.collectInstance(ctx, now, instID, group, withSize, b); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", instID, err))
		}
	}
	// Instances with no active project left, and connections idle for a
	// while, are let go.
	idle := max(3*c.interval, 5*time.Minute)
	c.closeConns(func(id uuid.UUID, ic *instanceConn) bool {
		_, live := byInstance[id]
		return !live || now.Sub(ic.used) > idle
	})

	if c.pooler == nil {
		return errors.Join(errs...)
	}
	// PgBouncer's view: clients holding or waiting for a server connection,
	// from the pooler hosts, which this server reads, and from every
	// server's readings of the poolers next to it.
	clients, waiting, moved := map[string]int64{}, map[string]int64{}, map[string]bool{}
	for _, r := range c.readings(ctx, &errs) {
		for db, v := range r.pools {
			clients[db] += v.Clients
			waiting[db] += v.Waiting
			if c.xactsMoved(now, r.source+"/"+db, v.Xacts) {
				moved[db] = true
			}
		}
	}
	var busyProjects []uuid.UUID
	for _, p := range ps {
		var cl, wt int64
		busy := false
		for _, name := range store.PoolerNames(p) {
			cl, wt = cl+clients[name], wt+waiting[name]
			busy = busy || moved[name]
		}
		b.add(p.ID, PoolerClients, float64(cl))
		b.add(p.ID, PoolerWaiting, float64(wt))
		if cl > 0 || busy {
			busyProjects = append(busyProjects, p.ID)
		}
	}
	// Client connections are what keeps a Free project awake (V3 §4.2);
	// Xenibase's own sessions (backups, metrics) don't go through the
	// poolers, so they don't count.
	q := store.New(c.db)
	if len(busyProjects) > 0 {
		if err := q.TouchProjectsActive(ctx, busyProjects); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := q.PrunePoolerReadings(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// poolCounts is one database's line in a pooler reading.
type poolCounts struct {
	Clients int64 `json:"c"` // active and waiting clients
	Waiting int64 `json:"w"`
	Xacts   int64 `json:"x"` // the pooler's transaction counter
}

// reading is one pooler's counts per database.
type reading struct {
	source string
	pools  map[string]poolCounts
}

// readAdmin is one pooler's SHOW POOLS and transaction counters.
func readAdmin(ctx context.Context, a *pooler.Admin) (map[string]poolCounts, error) {
	pools, err := a.Pools(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]poolCounts{}
	for _, p := range pools {
		v := out[p.Database]
		v.Clients += p.ClActive + p.ClWaiting
		v.Waiting += p.ClWaiting
		out[p.Database] = v
	}
	counts, err := a.Transactions(ctx)
	if err != nil {
		return nil, err
	}
	for db, n := range counts {
		v := out[db]
		v.Xacts = n
		out[db] = v
	}
	return out, nil
}

// publishLocal reads the poolers next to this server and leaves the
// readings for the collecting server, which may be this one.
func (c *Collector) publishLocal(ctx context.Context) error {
	if c.pooler == nil {
		return nil
	}
	var errs []error
	for _, a := range c.pooler.LocalAdmins() {
		pools, err := readAdmin(ctx, a)
		if err != nil {
			errs = append(errs, err)
			continue // its last reading ages out
		}
		raw, err := json.Marshal(pools)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := store.New(c.db).UpsertPoolerReading(ctx, store.UpsertPoolerReadingParams{
			Source: c.source + "/" + a.Name + "/" + a.Addr(), Pools: raw,
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// readings are the pooler hosts' counts, read now, and every server's
// readings of its own poolers from the last two intervals.
func (c *Collector) readings(ctx context.Context, errs *[]error) []reading {
	var out []reading
	for _, a := range c.pooler.HostAdmins() {
		pools, err := readAdmin(ctx, a)
		if err != nil {
			*errs = append(*errs, err)
			continue
		}
		out = append(out, reading{source: a.Name + "/" + a.Addr(), pools: pools})
	}
	rows, err := store.New(c.db).FreshPoolerReadings(ctx, (2*c.interval + 30*time.Second).Seconds())
	if err != nil {
		*errs = append(*errs, err)
		return out
	}
	for _, r := range rows {
		var pools map[string]poolCounts
		if err := json.Unmarshal(r.Pools, &pools); err != nil {
			*errs = append(*errs, fmt.Errorf("pooler reading %s: %w", r.Source, err))
			continue
		}
		out = append(out, reading{source: r.Source, pools: pools})
	}
	return out
}

// xactsMoved reports whether a pooler's transaction counter for a database
// went up since the last sample. A counter going down (a pooler restart)
// only resets the baseline, and so does one too old to compare, as when
// this server collects again after another did.
func (c *Collector) xactsMoved(now time.Time, key string, n int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.prevXacts[key]
	c.prevXacts[key] = xactCount{at: now, n: n}
	return ok && n > prev.n && now.Sub(prev.at) <= 3*c.interval+time.Minute
}

// conn is the collector's connection to an instance, opened when there is
// none or the instance's address or credentials changed; reused says it
// was kept from an earlier sample. The caller unlocks ic.mu when done.
func (c *Collector) conn(ctx context.Context, instID uuid.UUID) (ic *instanceConn, reused bool, err error) {
	cfg, err := c.projects.AdminConfig(ctx, instID, "postgres")
	if err != nil {
		return nil, false, err
	}
	key := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s", cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database)
	c.connMu.Lock()
	ic = c.conns[instID]
	if ic == nil {
		ic = &instanceConn{}
		c.conns[instID] = ic
	}
	c.connMu.Unlock()
	ic.mu.Lock()
	if ic.conn != nil && (ic.conn.IsClosed() || ic.key != key) {
		_ = ic.conn.Close(context.Background())
		ic.conn = nil
	}
	reused = ic.conn != nil
	if ic.conn == nil {
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			ic.mu.Unlock()
			return nil, false, err
		}
		ic.conn, ic.key = conn, key
	}
	ic.used = time.Now()
	return ic, reused, nil
}

// closeConns closes and forgets the instance connections drop picks,
// leaving any in use by a concurrent Collect.
func (c *Collector) closeConns(drop func(uuid.UUID, *instanceConn) bool) {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	for id, ic := range c.conns {
		if !ic.mu.TryLock() {
			continue
		}
		if drop(id, ic) {
			if ic.conn != nil {
				_ = ic.conn.Close(context.Background())
			}
			delete(c.conns, id)
		}
		ic.mu.Unlock()
	}
}

// collectInstance samples an instance's projects over the collector's
// connection to it. A kept connection that the instance closed meanwhile
// (a restart, a failover, the reaper) is opened again, once.
func (c *Collector) collectInstance(ctx context.Context, now time.Time, instID uuid.UUID, group []store.Project, withSize bool, b *batch) error {
	for attempt := 0; ; attempt++ {
		ic, reused, err := c.conn(ctx, instID)
		if err != nil {
			return err
		}
		var part batch
		err = c.sampleInstance(ctx, now, ic.conn, group, withSize, &part)
		broken := err != nil && ic.conn.IsClosed()
		if broken {
			ic.conn = nil
		}
		ic.mu.Unlock()
		if broken && reused && attempt == 0 && ctx.Err() == nil {
			continue
		}
		b.addAll(part)
		return err
	}
}

func (c *Collector) sampleInstance(ctx context.Context, now time.Time, conn *pgx.Conn, group []store.Project, withSize bool, b *batch) error {
	names := make([]string, len(group))
	byName := map[string]store.Project{}
	for i, p := range group {
		names[i] = p.DbName
		byName[p.DbName] = p
	}
	var archErr error
	if len(group) == 1 && group[0].Tier == provision.TierDedicated {
		if archErr = collectArchiver(ctx, conn, group[0].ID, b); archErr != nil {
			archErr = fmt.Errorf("wal archiver: %w", archErr)
		}
	}
	rows, err := conn.Query(ctx, `
		SELECT d.datname, s.xact_commit + s.xact_rollback, s.blks_hit, s.blks_read,
		       (SELECT count(*) FROM pg_stat_activity a WHERE a.datid = d.oid AND a.backend_type = 'client backend' AND a.state = 'active'),
		       (SELECT count(*) FROM pg_stat_activity a WHERE a.datid = d.oid AND a.backend_type = 'client backend' AND a.state IS DISTINCT FROM 'active'),
		       CASE WHEN $2 THEN pg_database_size(d.oid) END
		FROM pg_database d JOIN pg_stat_database s ON s.datid = d.oid
		WHERE d.datname = ANY($1)`, names, withSize)
	if err != nil {
		return err
	}
	defer rows.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	for rows.Next() {
		var name string
		var cur dbCounters
		var active, idle int64
		var size *int64
		if err := rows.Scan(&name, &cur.xacts, &cur.hits, &cur.reads, &active, &idle, &size); err != nil {
			return err
		}
		p := byName[name]
		cur.at = now
		b.add(p.ID, ConnectionsActive, float64(active))
		b.add(p.ID, ConnectionsIdle, float64(idle))
		if size != nil {
			b.add(p.ID, SizeBytes, float64(*size))
		}
		// Rates need the previous sample; counters going backwards mean a
		// stats reset or a move to another instance.
		if prev, ok := c.prevDB[p.ID]; ok && cur.xacts >= prev.xacts && cur.hits >= prev.hits && cur.reads >= prev.reads {
			if dt := cur.at.Sub(prev.at).Seconds(); dt > 0 {
				b.add(p.ID, TPS, float64(cur.xacts-prev.xacts)/dt)
			}
			if blocks := (cur.hits - prev.hits) + (cur.reads - prev.reads); blocks > 0 {
				b.add(p.ID, CacheHitRatio, float64(cur.hits-prev.hits)/float64(blocks))
			}
		}
		c.prevDB[p.ID] = cur
	}
	return errors.Join(rows.Err(), archErr)
}

// collectArchiver samples a dedicated instance's WAL archiving (its
// leader's, for HA: the instance's address follows it), for the alert on
// archiving that fails or falls behind: pg_stat_archiver's failed attempts
// and age of the last archived segment, and the segments waiting
// (.ready in archive_status) with how long the oldest has waited. Nothing
// waiting means nothing to archive, however old the last archived one.
func collectArchiver(ctx context.Context, conn *pgx.Conn, project uuid.UUID, b *batch) error {
	var on bool
	var failed, pending int64
	var lastAge, pendingAge *float64
	err := conn.QueryRow(ctx, `
		SELECT current_setting('archive_mode') <> 'off', a.failed_count,
		       EXTRACT(epoch FROM now() - a.last_archived_time)::float8,
		       (SELECT count(*) FROM pg_ls_archive_statusdir() s WHERE s.name LIKE '%.ready'),
		       (SELECT EXTRACT(epoch FROM now() - min(s.modification))::float8 FROM pg_ls_archive_statusdir() s WHERE s.name LIKE '%.ready')
		FROM pg_stat_archiver a`).Scan(&on, &failed, &lastAge, &pending, &pendingAge)
	if err != nil || !on {
		return err
	}
	b.add(project, WALArchiveFailed, float64(failed))
	b.add(project, WALArchivePending, float64(pending))
	b.add(project, WALArchivePendingAge, max(ptrOr(pendingAge), 0))
	if lastAge != nil {
		b.add(project, WALArchiveAge, *lastAge)
	}
	return nil
}

func ptrOr(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func (c *Collector) collectNodes(ctx context.Context, now time.Time, b *batch) error {
	if c.nodes == nil {
		return nil
	}
	ns, err := c.nodes.List(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range ns {
		st, ok := c.nodes.Status(n.ID)
		if !ok || !st.Reachable || now.Sub(st.CheckedAt) > 2*c.interval+time.Minute {
			continue
		}
		m := st.Metrics
		b.add(n.ID, Load1, m.Load1)
		if m.MemTotalBytes > 0 {
			b.add(n.ID, MemTotalBytes, float64(m.MemTotalBytes))
			b.add(n.ID, MemUsedBytes, float64(m.MemTotalBytes-m.MemAvailableBytes))
		}
		if m.DiskTotalBytes > 0 {
			b.add(n.ID, DiskTotalBytes, float64(m.DiskTotalBytes))
			b.add(n.ID, DiskUsedBytes, float64(m.DiskTotalBytes-m.DiskFreeBytes))
		}
		cur := nodeCounters{at: st.CheckedAt, busy: m.CPUBusyTicks, total: m.CPUTotalTicks, read: m.DiskReadBytes, writ: m.DiskWriteBytes}
		prev, had := c.prevNode[n.ID]
		if had && cur.at.After(prev.at) {
			if cur.total > prev.total && cur.busy >= prev.busy {
				b.add(n.ID, CPUPercent, 100*float64(cur.busy-prev.busy)/float64(cur.total-prev.total))
			}
			dt := cur.at.Sub(prev.at).Seconds()
			if cur.read >= prev.read && cur.writ >= prev.writ && (cur.read > 0 || cur.writ > 0) {
				b.add(n.ID, DiskReadBPS, float64(cur.read-prev.read)/dt)
				b.add(n.ID, DiskWriteBPS, float64(cur.writ-prev.writ)/dt)
			}
		}
		if !had || cur.at.After(prev.at) {
			c.prevNode[n.ID] = cur
		}
	}
	return nil
}
