package metrics

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Ranges the API serves: 1h and 24h from 1-minute points, 7d from hourly.
var Ranges = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

// Point is one value at a time.
type Point struct {
	TS    time.Time
	Value float64
}

// Series is one metric's points, oldest first.
type Series struct {
	Metric string
	Points []Point
}

// Resolution is the point resolution a range is served at.
func Resolution(rng string) string {
	if rng == "7d" {
		return "1h"
	}
	return "1m"
}

// Query returns the series for one scope over a range. The 7-day range
// adds averages of the hours not yet downsampled.
func Query(ctx context.Context, db store.DBTX, scope string, id uuid.UUID, rng string, names []string) ([]Series, error) {
	d, ok := Ranges[rng]
	if !ok {
		return nil, fmt.Errorf("unknown range %q", rng)
	}
	if names == nil {
		names = []string{}
	}
	q := store.New(db)
	res := Resolution(rng)
	since := time.Now().Add(-d)
	rows, err := q.MetricSeries(ctx, store.MetricSeriesParams{Scope: scope, ScopeID: id, Resolution: res, Since: since, Metrics: names})
	if err != nil {
		return nil, err
	}
	byMetric := map[string][]Point{}
	for _, r := range rows {
		byMetric[r.Metric] = append(byMetric[r.Metric], Point{TS: r.Ts, Value: r.Value})
	}
	if res == "1h" {
		recent, err := q.MetricSeries(ctx, store.MetricSeriesParams{
			Scope: scope, ScopeID: id, Resolution: "1m", Since: time.Now().Add(-3 * time.Hour).Truncate(time.Hour), Metrics: names,
		})
		if err != nil {
			return nil, err
		}
		type key struct {
			metric string
			hour   time.Time
		}
		sum, n := map[key]float64{}, map[key]int{}
		for _, r := range recent {
			k := key{r.Metric, r.Ts.Truncate(time.Hour)}
			sum[k] += r.Value
			n[k]++
		}
		for k, total := range sum {
			have := false
			for _, p := range byMetric[k.metric] {
				if p.TS.Equal(k.hour) {
					have = true
					break
				}
			}
			if !have {
				byMetric[k.metric] = append(byMetric[k.metric], Point{TS: k.hour, Value: total / float64(n[k])})
			}
		}
	}
	out := make([]Series, 0, len(byMetric))
	for m, pts := range byMetric {
		sort.Slice(pts, func(i, j int) bool { return pts[i].TS.Before(pts[j].TS) })
		out = append(out, Series{Metric: m, Points: pts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metric < out[j].Metric })
	return out, nil
}

// TopQuery is one pg_stat_statements entry.
type TopQuery struct {
	Query   string
	Calls   int64
	TotalMS float64
	MeanMS  float64
	Rows    int64
}

// TopQueries returns the project database's 10 queries with the most total
// time from pg_stat_statements, or available=false when the extension is
// not enabled there (spec §8.7). The control plane's own queries are left
// out.
func TopQueries(ctx context.Context, projects *provision.Service, p store.Project) (available bool, out []TopQuery, err error) {
	conn, err := projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return false, nil, err
	}
	defer conn.Close(context.Background())
	var schema *string
	if err := conn.QueryRow(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname = 'pg_stat_statements'`).Scan(&schema); err != nil || schema == nil {
		return false, []TopQuery{}, nil //nolint:nilerr // not installed
	}
	rows, err := conn.Query(ctx, `SELECT query, calls, total_exec_time, mean_exec_time, rows
		FROM `+provision.Ident(*schema)+`.pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND userid <> (SELECT oid FROM pg_roles WHERE rolname = current_user)
		ORDER BY total_exec_time DESC LIMIT 10`)
	if err != nil {
		return true, nil, err
	}
	defer rows.Close()
	out = []TopQuery{}
	for rows.Next() {
		var t TopQuery
		if err := rows.Scan(&t.Query, &t.Calls, &t.TotalMS, &t.MeanMS, &t.Rows); err != nil {
			return true, nil, err
		}
		out = append(out, t)
	}
	return true, out, rows.Err()
}

// promHelp describes each exported metric.
var promHelp = map[string]string{
	SizeBytes:         "Database size in bytes.",
	ConnectionsActive: "Backends running a query.",
	ConnectionsIdle:   "Idle backends (including idle in transaction).",
	PoolerClients:     "Clients holding or waiting for a server connection in the poolers.",
	PoolerWaiting:     "Clients waiting for a server connection in the poolers.",
	TPS:               "Transactions (commits and rollbacks) per second.",
	CacheHitRatio:     "Share of block reads served from shared buffers.",
	// Dedicated projects only.
	WALArchiveFailed:     "Failed WAL archiving attempts since the statistics were reset.",
	WALArchiveAge:        "Seconds since a WAL segment was last archived.",
	WALArchivePending:    "Completed WAL segments waiting to be archived.",
	WALArchivePendingAge: "Seconds the oldest WAL segment waiting to be archived has waited (0 when none waits).",
	CPUPercent:           "CPU busy percentage.",
	Load1:                "1-minute load average.",
	MemUsedBytes:         "Memory in use (total less available).",
	MemTotalBytes:        "Total memory.",
	DiskUsedBytes:        "Used bytes on the data disk.",
	DiskTotalBytes:       "Size of the data disk.",
	DiskReadBPS:          "Disk read bytes per second.",
	DiskWriteBPS:         "Disk written bytes per second.",
	// Instances (shared clusters and dedicated instances).
	WebhookListenConnections: "Direct connections the webhook deliverer holds LISTENing on the instance's databases, outside the poolers.",
}

// WritePrometheus writes the latest point of every metric in the text
// exposition format, with project, node and instance labels.
//
// Project series are labelled by the opaque project id, the tier, and the
// owning organisation, never by the project's or database's name: whoever
// scrapes this (the platform admin, or a Prometheus holding the metrics
// token) may see organisations, but names inside one are the tenant's own
// (V2 s2.4).
func WritePrometheus(ctx context.Context, w io.Writer, db store.DBTX, interval time.Duration) error {
	q := store.New(db)
	window := max(3*interval, 6*time.Minute) // sizes come every 5 minutes
	latest, err := q.LatestMetrics(ctx, time.Now().Add(-window))
	if err != nil {
		return err
	}
	labels := map[uuid.UUID]string{}
	orgs, err := q.ListOrgNames(ctx)
	if err != nil {
		return err
	}
	orgName := make(map[uuid.UUID]string, len(orgs))
	for _, o := range orgs {
		orgName[o.ID] = o.Name
	}
	ps, err := q.ListLiveProjects(ctx, store.ListLiveProjectsParams{MaxRows: 100000})
	if err != nil {
		return err
	}
	for _, p := range ps {
		labels[p.ID] = fmt.Sprintf(`project_id="%s",org_id="%s",org=%s,tier=%s`, p.ID, p.OrgID, quote(orgName[p.OrgID]), quote(p.Tier))
	}
	ns, err := q.ListNodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range ns {
		labels[n.ID] = fmt.Sprintf(`node_id="%s",node=%s`, n.ID, quote(n.Name))
	}
	insts, err := q.ListInstanceLabels(ctx)
	if err != nil {
		return err
	}
	for _, i := range insts {
		labels[i.ID] = fmt.Sprintf(`instance_id="%s",kind=%s,node_id="%s",node=%s`, i.ID, quote(i.Kind), i.NodeID, quote(i.Node))
	}

	type sample struct {
		labels string
		value  float64
	}
	byName, metricOf := map[string][]sample{}, map[string]string{}
	for _, r := range latest {
		l, ok := labels[r.ScopeID]
		if !ok {
			continue // deleted since
		}
		name := "xenibase_" + r.Scope + "_" + r.Metric
		byName[name] = append(byName[name], sample{l, r.Value})
		metricOf[name] = r.Metric
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# HELP xenibase_up Xenibase control plane is serving.\n# TYPE xenibase_up gauge\nxenibase_up 1\n")
	for _, name := range names {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, promHelp[metricOf[name]], name)
		ss := byName[name]
		sort.Slice(ss, func(i, j int) bool { return ss[i].labels < ss[j].labels })
		for _, s := range ss {
			fmt.Fprintf(&b, "%s{%s} %s\n", name, s.labels, formatFloat(s.value))
		}
	}
	_, err = io.WriteString(w, b.String())
	return err
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
