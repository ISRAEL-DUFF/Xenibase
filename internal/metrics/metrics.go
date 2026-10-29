// Package metrics samples project and node metrics into metric_points,
// downsamples them, and serves series and the Prometheus exposition
// (spec §8.7).
package metrics

import (
	"context"
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

// sizeEvery is how often database sizes are sampled (spec §8.7: 5 min);
// pg_database_size walks the whole directory.
const sizeEvery = 5 * time.Minute

// Collector samples metrics on an interval.
type Collector struct {
	db       *pgxpool.Pool
	projects *provision.Service
	pooler   *pooler.Manager
	nodes    *nodes.Service
	interval time.Duration
	log      *slog.Logger

	mu     sync.Mutex
	prevDB map[uuid.UUID]dbCounters
	// prevXacts are the poolers' transaction counters per database at the
	// last sample, keyed by pooler and database (activity, V3 §4.2).
	prevXacts map[string]int64
	prevNode  map[uuid.UUID]nodeCounters
	lastSize  time.Time
}

type dbCounters struct {
	at                 time.Time
	xacts, hits, reads int64
}

type nodeCounters struct {
	at                      time.Time
	busy, total, read, writ uint64
}

// NewCollector returns a Collector. nodes may be nil (no agents).
func NewCollector(db *pgxpool.Pool, projects *provision.Service, pm *pooler.Manager, ns *nodes.Service, interval time.Duration, log *slog.Logger) *Collector {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Collector{
		db: db, projects: projects, pooler: pm, nodes: ns, interval: interval, log: log,
		prevDB: map[uuid.UUID]dbCounters{}, prevNode: map[uuid.UUID]nodeCounters{}, prevXacts: map[string]int64{},
	}
}

// Interval is the sampling interval.
func (c *Collector) Interval() time.Duration { return c.interval }

// Run samples every interval and downsamples every hour until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	// After a restart, catch up on every hour 1-minute points still cover.
	nextDownsample, since := time.Now(), time.Now().Add(-25*time.Hour)
	for {
		if err := c.Collect(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("collect metrics", "err", err)
		}
		if time.Now().After(nextDownsample) {
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

// Collect takes one sample of every active project and every node.
func (c *Collector) Collect(ctx context.Context) error {
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

	// PgBouncer's view: clients holding or waiting for a server connection.
	clients, waiting := map[string]int64{}, map[string]int64{}
	if c.pooler != nil {
		for _, a := range c.pooler.Admins() {
			pools, err := a.Pools(ctx)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, p := range pools {
				clients[p.Database] += p.ClActive + p.ClWaiting
				waiting[p.Database] += p.ClWaiting
			}
		}
		moved := c.transactionsMoved(ctx, &errs)
		var active []uuid.UUID
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
				active = append(active, p.ID)
			}
		}
		// Client connections are what keeps a Free project awake (V3 §4.2);
		// Xenibase's own sessions (backups, metrics) don't go through the
		// poolers, so they don't count.
		if len(active) > 0 {
			if err := store.New(c.db).TouchProjectsActive(ctx, active); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// transactionsMoved reports the databases whose pooler transaction
// counters went up since the last sample. A counter going down (a pooler
// restart) only resets the baseline.
func (c *Collector) transactionsMoved(ctx context.Context, errs *[]error) map[string]bool {
	out := map[string]bool{}
	for _, a := range c.pooler.Admins() {
		counts, err := a.Transactions(ctx)
		if err != nil {
			*errs = append(*errs, err)
			continue
		}
		c.mu.Lock()
		for db, n := range counts {
			key := a.Name + "/" + a.Addr() + "/" + db
			if prev, ok := c.prevXacts[key]; ok && n > prev {
				out[db] = true
			}
			c.prevXacts[key] = n
		}
		c.mu.Unlock()
	}
	return out
}

func (c *Collector) collectInstance(ctx context.Context, now time.Time, instID uuid.UUID, group []store.Project, withSize bool, b *batch) error {
	conn, err := c.projects.AdminConn(ctx, instID, "postgres")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
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
