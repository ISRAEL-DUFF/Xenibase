package alerts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/faildomain"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/store"
)

// Alert kinds (spec §8.8), plus a failed weekly isolation check (§7.1).
const (
	KindBackupFailed      = "backup_failed"
	KindBackupOverdue     = "backup_overdue"
	KindRestoreTestFailed = "restore_test_failed"
	KindNodeDisk          = "node_disk"
	KindNodeUnreachable   = "node_unreachable"
	KindProjectDisk       = "project_disk"
	KindPoolerDown        = "pooler_down"
	// Standby edge pooler (V3 §2.1).
	KindPoolerHostNotReady = "pooler_host_not_ready"
	KindPoolerSplitBrain   = "pooler_split_brain"
	// A pooler ignores hba.conf (auth_type is not hba): IP allowlists are
	// stored but not enforced.
	KindPoolerAllowlistOff = "pooler_allowlist_off"
	// The last pooler sync left a row out as invalid (a bad node address,
	// say): those projects can't connect through the poolers.
	KindPoolerSkipped  = "pooler_config_skipped"
	KindIsolationCheck = "isolation_check_failed"
	// Capacity automation (V3 §5.2): a proposal waits for approval, or a
	// provisioning failed in the last day.
	// HA members, etcd members or pooler hosts share a failure domain (V3.1 §2.4).
	KindFailureDomain = "failure_domain"
	// The Free tier's waker (V3 §4.2) doesn't accept connections.
	KindWakerDown          = "waker_down"
	KindCapacityProposal   = "capacity_proposal"
	KindCapacityFailed     = "capacity_failed"
	SeverityWarning        = "warning"
	SeverityCritical       = "critical"
	defaultInterval        = 30 * time.Second
	backupOverdueAfter     = 26 * time.Hour
	nodeDiskThreshold      = 0.85
	nodeUnreachableAfter   = 2 * time.Minute
	projectSizeFreshWithin = 30 * time.Minute
	clientCertWarnWithin   = 3 * 24 * time.Hour
	// xenibase-server's client certificate for agents is close to expiry:
	// renewing it keeps failing.
	KindAgentClientCert = "agent_client_cert"
	// A Postgres minor upgrade of an instance failed (V3 §2.4): the sweep
	// leaves the instance on its release until a newer one or Run now.
	KindMinorUpgradeFailed = "minor_upgrade_failed"
)

// Config configures the service.
type Config struct {
	// Interval is how often conditions are evaluated (default 30s).
	Interval time.Duration
	// PublicURL is the web UI's address, for links in notifications.
	PublicURL string
	// Poolers are checked for "pooler down".
	Poolers []*pooler.Admin
	// PoolerGrace is how long a pooler must keep failing before it is down
	// (default 1 min: at startup the poolers come up after the server).
	PoolerGrace time.Duration
	// WakerAddr returns the waker's address as the poolers reach it ("" when
	// there is none); checked for "waker down" with the poolers' grace.
	WakerAddr func() string
	// PoolerSkipped returns what the last pooler sync left out as invalid.
	PoolerSkipped func() []string
	// ClientCertExpiry returns when xenibase-server's agent client
	// certificate expires (renewing it first when due); nil skips the check.
	ClientCertExpiry func() time.Time
}

// Service evaluates conditions and delivers notifications.
type Service struct {
	db      *pgxpool.Pool
	keyring *crypto.Keyring
	cfg     Config
	http    *http.Client
	log     *slog.Logger

	mu            sync.Mutex
	poolerFailing map[string]time.Time // since when each pooler fails
}

// New returns a Service.
func New(db *pgxpool.Pool, keyring *crypto.Keyring, cfg Config, log *slog.Logger) *Service {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultInterval
	}
	if cfg.PoolerGrace == 0 {
		cfg.PoolerGrace = time.Minute
	}
	return &Service{db: db, keyring: keyring, cfg: cfg, http: &http.Client{Timeout: 15 * time.Second}, log: log,
		poolerFailing: map[string]time.Time{}}
}

// Run evaluates and delivers every interval until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("alerts", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick evaluates every condition once, then delivers pending notifications.
func (s *Service) Tick(ctx context.Context) error {
	err := s.Evaluate(ctx)
	return errors.Join(err, s.DeliverPending(ctx))
}

// condition is one alert that should be firing now.
type condition struct {
	kind, severity, targetType, targetID, targetName, summary string
	detail                                                    map[string]any
}

func (c condition) key() string { return c.kind + ":" + c.targetType + ":" + c.targetID }

// Evaluate fires alerts for the conditions that hold and resolves the ones
// that no longer do. Each transition is recorded once, whichever server
// sees it first.
func (s *Service) Evaluate(ctx context.Context) error {
	conds, err := s.conditions(ctx)
	if err != nil {
		return err
	}
	q := store.New(s.db)
	now := map[string]bool{}
	for _, c := range conds {
		now[c.key()] = true
		detail, _ := json.Marshal(c.detail)
		row, err := q.FireAlert(ctx, store.FireAlertParams{
			Kind: c.kind, Key: c.key(), Severity: c.severity, TargetType: c.targetType, TargetID: c.targetID,
			TargetName: c.targetName, Summary: c.summary, Detail: detail,
		})
		if err != nil {
			return err
		}
		if row.Inserted {
			s.log.Warn("alert firing", "kind", c.kind, "target", c.targetName, "summary", c.summary, "alert_id", row.ID)
		}
	}
	firing, err := q.FiringAlerts(ctx)
	if err != nil {
		return err
	}
	for _, a := range firing {
		if now[a.Key] {
			continue
		}
		if n, err := q.ResolveAlert(ctx, a.ID); err != nil {
			return err
		} else if n > 0 {
			s.log.Info("alert resolved", "kind", a.Kind, "target", a.TargetName, "alert_id", a.ID)
		}
	}
	return nil
}

func (s *Service) conditions(ctx context.Context) ([]condition, error) {
	q := store.New(s.db)
	var out []condition
	var errs []error

	names := map[uuid.UUID]string{}
	if ps, err := q.ListLiveProjects(ctx, store.ListLiveProjectsParams{MaxRows: 100000}); err == nil {
		for _, p := range ps {
			names[p.ID] = p.Name
		}
	} else {
		errs = append(errs, err)
	}

	// Backups and restore tests whose latest run failed; isolation checks too.
	ops, err := q.FailedLatestOperations(ctx, []string{"backup", "base_backup", "metadata_backup", "restore_test", "isolation_check"})
	if err != nil {
		errs = append(errs, err)
	}
	nodeOf := map[string]string{}
	if insts, err := q.ListLiveSharedInstances(ctx); err == nil {
		for _, i := range insts {
			nodeOf[i.ID.String()] = i.NodeName
		}
	}
	for _, o := range ops {
		if o.Status != "failed" {
			continue
		}
		msg := ""
		if o.Error != nil {
			msg = *o.Error
		}
		detail := map[string]any{"operation_id": o.ID, "error": msg}
		switch o.Kind {
		case "metadata_backup":
			out = append(out, condition{KindBackupFailed, SeverityCritical, "control_plane", "metadata", "metadata database",
				"Nightly backup of the metadata database failed: " + msg, detail})
		case "backup", "base_backup":
			if o.ProjectID == nil {
				continue
			}
			name, live := names[*o.ProjectID]
			if !live {
				continue
			}
			out = append(out, condition{KindBackupFailed, SeverityCritical, "project", o.ProjectID.String(), name,
				fmt.Sprintf("Backup of %s failed: %s", name, msg), detail})
		case "restore_test":
			id, name := "all", "restore test"
			if o.ProjectID != nil {
				id, name = o.ProjectID.String(), names[*o.ProjectID]
			}
			out = append(out, condition{KindRestoreTestFailed, SeverityCritical, "project", id, name,
				fmt.Sprintf("Restore test of %s failed: %s", name, msg), detail})
		case "isolation_check":
			node := nodeOf[o.InstanceID]
			if node == "" {
				continue // the cluster is gone
			}
			out = append(out, condition{KindIsolationCheck, SeverityCritical, "instance", o.InstanceID, "shared cluster on " + node,
				"Tenant isolation check failed on " + node + ": " + msg, detail})
		}
	}

	minors, err := q.FailedMinorUpgrades(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, m := range minors {
		what := m.Kind + " instance on " + m.NodeName
		out = append(out, condition{KindMinorUpgradeFailed, SeverityCritical, "instance", m.InstanceID.String(), what,
			fmt.Sprintf("Minor upgrade of the %s from %s to %s failed: %s", what, m.FromRelease, m.ToRelease, m.Error),
			map[string]any{"minor_upgrade_id": m.ID, "to_release": m.ToRelease, "error": m.Error}})
	}

	overdue, err := q.OverdueBackups(ctx, backupOverdueAfter.Seconds())
	if err != nil {
		errs = append(errs, err)
	}
	for _, p := range overdue {
		since := "never backed up"
		if p.LastBackupAt.Year() > 1970 {
			since = "last backup " + p.LastBackupAt.UTC().Format(time.RFC3339)
		}
		out = append(out, condition{KindBackupOverdue, SeverityWarning, "project", p.ID.String(), p.Name,
			fmt.Sprintf("No backup of %s in 26 hours (%s)", p.Name, since), map[string]any{"last_backup_at": p.LastBackupAt}})
	}

	unreachable, err := q.UnreachableNodes(ctx, nodeUnreachableAfter.Seconds())
	if err != nil {
		errs = append(errs, err)
	}
	down := map[uuid.UUID]bool{}
	for _, n := range unreachable {
		down[n.ID] = true
		since := "never reached"
		if n.LastReachableAt != nil {
			since = "unreachable since " + n.LastReachableAt.UTC().Format(time.RFC3339)
		}
		out = append(out, condition{KindNodeUnreachable, SeverityCritical, "node", n.ID.String(), n.Name,
			fmt.Sprintf("Node %s's agent has not answered for 2+ minutes (%s)", n.Name, since), map[string]any{"last_reachable_at": n.LastReachableAt}})
	}

	caps, err := q.NodeCapacities(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, n := range caps {
		var m agentapi.HostMetrics
		if down[n.ID] || json.Unmarshal(n.Capacity, &m) != nil || m.DiskTotalBytes <= 0 {
			continue
		}
		used := float64(m.DiskTotalBytes-m.DiskFreeBytes) / float64(m.DiskTotalBytes)
		if used > nodeDiskThreshold {
			out = append(out, condition{KindNodeDisk, SeverityWarning, "node", n.ID.String(), n.Name,
				fmt.Sprintf("Node %s's disk is %.0f%% full (%s)", n.Name, used*100, m.DiskPath),
				map[string]any{"used_ratio": used, "disk_total_bytes": m.DiskTotalBytes, "disk_free_bytes": m.DiskFreeBytes}})
		}
	}

	sizes, err := q.ProjectSizes(ctx, time.Now().Add(-projectSizeFreshWithin))
	if err != nil {
		errs = append(errs, err)
	}
	for _, p := range sizes {
		set, err := store.DecodeProjectSettings(p.Settings)
		if err != nil || p.SizeBytes < 0 || set.DiskWarnBytes <= 0 || p.SizeBytes <= float64(set.DiskWarnBytes) {
			continue
		}
		out = append(out, condition{KindProjectDisk, SeverityWarning, "project", p.ID.String(), p.Name,
			fmt.Sprintf("%s is %s, over its disk warning of %s", p.Name, bytesStr(p.SizeBytes), bytesStr(float64(set.DiskWarnBytes))),
			map[string]any{"size_bytes": p.SizeBytes, "disk_warn_bytes": set.DiskWarnBytes}})
	}

	if props, err := q.ListCapacityProposals(ctx); err == nil {
		for _, p := range props {
			detail := map[string]any{"proposal_id": p.ID, "server_type": p.ServerType, "monthly_cost_minor": p.MonthlyCostMinor, "currency": p.Currency}
			switch {
			case p.Status == "pending":
				out = append(out, condition{KindCapacityProposal, SeverityWarning, "region", p.Region + "/" + p.Tier, p.Region + " " + p.Tier,
					"A capacity proposal waits for approval (Platform → Capacity): " + p.Reason, detail})
			case p.Status == "failed" && time.Since(p.UpdatedAt) < 24*time.Hour:
				msg := ""
				if p.Error != nil {
					msg = *p.Error
				}
				out = append(out, condition{KindCapacityFailed, SeverityCritical, "region", p.Region + "/" + p.Tier, p.Region + " " + p.Tier,
					"Provisioning a " + p.ServerType + " server failed: " + msg, detail})
			}
		}
	} else {
		errs = append(errs, err)
	}

	out = append(out, s.poolerConditions(ctx)...)
	out = append(out, s.wakerConditions(ctx)...)
	out = append(out, skippedConditions(s.cfg.PoolerSkipped)...)
	out = append(out, s.clientCertConditions()...)
	hostConds, err := s.poolerHostConditions(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	out = append(out, hostConds...)
	// Groups that must be apart but share a failure domain (V3.1 §2.4).
	if probs, err := faildomain.Check(ctx, q); err == nil {
		for _, p := range probs {
			out = append(out, condition{KindFailureDomain, SeverityWarning, "failure_domain", p.Key, p.Region + " " + p.Group,
				p.Detail + ". One failure could take them all: move one, or correct the domains (Platform → Nodes).",
				map[string]any{"group": p.Group, "region": p.Region, "nodes": p.Nodes}})
		}
	} else {
		errs = append(errs, err)
	}
	// A partial evaluation must not resolve alerts it could not check.
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// poolerConditions reports poolers whose admin console has not answered
// for the grace period.
func (s *Service) poolerConditions(ctx context.Context) []condition {
	var out []condition
	for _, a := range s.cfg.Poolers {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := a.Ping(pctx)
		cancel()
		s.mu.Lock()
		since, failing := s.poolerFailing[a.Name]
		switch {
		case err == nil:
			delete(s.poolerFailing, a.Name)
		case !failing:
			since = time.Now()
			s.poolerFailing[a.Name] = since
		}
		s.mu.Unlock()
		if err != nil && ctx.Err() == nil && time.Since(since) >= s.cfg.PoolerGrace {
			out = append(out, condition{KindPoolerDown, SeverityCritical, "pooler", a.Name, a.Name + " pooler (" + a.Addr() + ")",
				fmt.Sprintf("The %s pooler at %s is down: %v", a.Name, a.Addr(), err), map[string]any{"address": a.Addr()}})
		}
		if err == nil {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			why, err := a.EnforcesHBA(pctx)
			cancel()
			if err == nil && why != "" {
				out = append(out, condition{KindPoolerAllowlistOff, SeverityCritical, "pooler", a.Name, a.Name + " pooler (" + a.Addr() + ")",
					why, map[string]any{"address": a.Addr()}})
			}
		}
	}
	return out
}

// skippedConditions reports rows the last pooler sync left out as invalid:
// the rest of the configuration was applied, but those routes or logins
// don't work until the row is fixed.
func skippedConditions(skipped func() []string) []condition {
	if skipped == nil {
		return nil
	}
	list := skipped()
	if len(list) == 0 {
		return nil
	}
	return []condition{{KindPoolerSkipped, SeverityCritical, "pooler", "config", "pooler configuration",
		fmt.Sprintf("The pooler configuration left out %d invalid entr%s, so those projects or logins can't connect through the poolers: %s. Fix the row (often a node's private address) and the next sync includes it.",
			len(list), map[bool]string{true: "y", false: "ies"}[len(list) == 1], strings.Join(list, "; ")),
		map[string]any{"skipped": list}}}
}

// wakerConditions reports a waker that hasn't accepted connections for the
// grace period: paused Free projects can't wake from a connection, and the
// sweep stops pausing until it is back.
func (s *Service) wakerConditions(ctx context.Context) []condition {
	if s.cfg.WakerAddr == nil {
		return nil
	}
	addr := s.cfg.WakerAddr()
	if addr == "" {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	c, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr)
	cancel()
	up := err == nil
	if up {
		_ = c.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	since, failing := s.poolerFailing["waker"]
	switch {
	case up:
		delete(s.poolerFailing, "waker")
		return nil
	case !failing:
		since = time.Now()
		s.poolerFailing["waker"] = since
	}
	if ctx.Err() != nil || time.Since(since) < s.cfg.PoolerGrace {
		return nil
	}
	return []condition{{KindWakerDown, SeverityCritical, "control_plane", "waker", "waker",
		fmt.Sprintf("The waker at %s doesn't accept connections: paused Free projects can't resume when a client connects (they can still be resumed from the dashboard), and pausing is on hold.", addr),
		map[string]any{"address": addr}}}
}

// clientCertConditions reports xenibase-server's agent client certificate
// when less than clientCertWarnWithin is left: it is renewed with a week
// left, so renewing has been failing. Once it expires no agent accepts the
// server.
func (s *Service) clientCertConditions() []condition {
	if s.cfg.ClientCertExpiry == nil {
		return nil
	}
	exp := s.cfg.ClientCertExpiry()
	if time.Until(exp) > clientCertWarnWithin {
		return nil
	}
	return []condition{{KindAgentClientCert, SeverityCritical, "control_plane", "agent_client_cert", "agent client certificate",
		fmt.Sprintf("xenibase-server's client certificate for agents expires %s and could not be renewed (see the server's log). When it expires, no agent accepts the server until it restarts.", exp.UTC().Format(time.RFC3339)),
		map[string]any{"expires_at": exp.UTC().Format(time.RFC3339)}}}
}

// poolerHostConditions reports pooler hosts (V3 §2.1) that answer but
// can't serve (a PgBouncer down, or a stale configuration) for the grace
// period, and split brain: more than one host keepalived MASTER. An
// unreachable host is already "node unreachable".
func (s *Service) poolerHostConditions(ctx context.Context) ([]condition, error) {
	hosts, err := store.New(s.db).PoolerHosts(ctx)
	if err != nil {
		return nil, err
	}
	fresh := time.Now().Add(-2 * time.Minute)
	var out []condition
	masters := map[string][]string{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range hosts {
		checked := h.PoolerCheckedAt != nil && h.PoolerCheckedAt.After(fresh)
		key := "host:" + h.ID.String()
		notReady := checked && h.Status == "healthy" && h.PoolerReady != nil && !*h.PoolerReady
		since, failing := s.poolerFailing[key]
		switch {
		case !notReady:
			delete(s.poolerFailing, key)
		case !failing:
			since = time.Now()
			s.poolerFailing[key] = since
		}
		if notReady && time.Since(since) >= s.cfg.PoolerGrace {
			out = append(out, condition{KindPoolerHostNotReady, SeverityCritical, "node", h.ID.String(), h.Name,
				fmt.Sprintf("Pooler host %s can't serve: a PgBouncer is down, its configuration is stale, or it ignores hba.conf (IP allowlists not enforced), so it can't take the floating IP. Admin → Nodes shows which.", h.Name),
				map[string]any{"generation": h.PoolerGeneration, "vrrp_state": h.PoolerVrrpState}})
		}
		if checked && h.Status == "healthy" && h.PoolerVrrpState != nil && *h.PoolerVrrpState == "MASTER" {
			masters[h.Region] = append(masters[h.Region], h.Name)
		}
	}
	// Each region's pair has its own floating IP, so split brain is two
	// MASTERs in one region; a MASTER in each region is normal (V3 §6.1).
	for key := range s.poolerFailing {
		if r, ok := strings.CutPrefix(key, "split:"); ok && len(masters[r]) < 2 {
			delete(s.poolerFailing, key)
		}
	}
	regions := make([]string, 0, len(masters))
	for r := range masters {
		regions = append(regions, r)
	}
	sort.Strings(regions)
	for _, r := range regions {
		if len(masters[r]) < 2 {
			continue
		}
		key := "split:" + r
		since, failing := s.poolerFailing[key]
		if !failing {
			since = time.Now()
			s.poolerFailing[key] = since
		}
		if time.Since(since) >= s.cfg.PoolerGrace {
			out = append(out, condition{KindPoolerSplitBrain, SeverityCritical, "pooler", "edge:" + r, r + " edge pooler",
				fmt.Sprintf("More than one pooler host in %s is keepalived MASTER (%s): check the network between them. xenibase-server keeps the floating IP on one healthy host.", r, strings.Join(masters[r], ", ")),
				map[string]any{"masters": masters[r], "region": r}})
		}
	}
	return out, nil
}

// DeliverPending sends notifications not yet delivered (new firings and
// resolutions, and earlier failures once their backoff passes).
func (s *Service) DeliverPending(ctx context.Context) error {
	q := store.New(s.db)
	pending, err := q.ClaimUndelivered(ctx)
	if err != nil || len(pending) == 0 {
		return err
	}
	ch, err := s.channels(ctx)
	if err != nil {
		return err
	}
	for _, a := range pending {
		resolved := a.Status == "resolved"
		if !ch.any() {
			if err := q.SkipDelivery(ctx, a.ID); err != nil {
				return err
			}
			continue
		}
		event := EventFiring
		if resolved {
			event = EventResolved
		}
		if err := s.deliver(ctx, ch, s.payload(event, a)); err != nil {
			s.log.Warn("alert delivery failed", "alert_id", a.ID, "event", event, "err", err)
			msg := truncate(err.Error(), 1000)
			if err := q.MarkDeliveryFailed(ctx, store.MarkDeliveryFailedParams{ID: a.ID, Error: &msg}); err != nil {
				return err
			}
			continue
		}
		if err := q.MarkDelivered(ctx, store.MarkDeliveredParams{ID: a.ID, Resolved: resolved}); err != nil {
			return err
		}
	}
	return nil
}

// ChannelResult is one channel's outcome of a test notification.
type ChannelResult struct {
	Channel string
	OK      bool
	Error   string
}

// SendTest sends a test notification to each configured channel.
func (s *Service) SendTest(ctx context.Context) ([]ChannelResult, error) {
	ch, err := s.channels(ctx)
	if err != nil {
		return nil, err
	}
	p := s.payload(EventTest, store.Alert{
		ID: uuid.New(), Kind: "test", Severity: SeverityWarning, Status: "firing", Summary: "Test alert from Xenibase",
		TargetType: "control_plane", TargetID: "test", TargetName: "Xenibase", StartedAt: time.Now(),
	})
	var out []ChannelResult
	if ch.webhookURL != "" {
		r := ChannelResult{Channel: "webhook", OK: true}
		if err := s.sendWebhook(ctx, ch, p); err != nil {
			r.OK, r.Error = false, err.Error()
		}
		out = append(out, r)
	}
	if ch.smtp != nil {
		r := ChannelResult{Channel: "email", OK: true}
		if err := sendMail(ctx, *ch.smtp, p); err != nil {
			r.OK, r.Error = false, err.Error()
		}
		out = append(out, r)
	}
	return out, nil
}

// List returns alerts, firing first.
func (s *Service) List(ctx context.Context, status *string, limit int) ([]store.Alert, error) {
	return store.New(s.db).ListAlerts(ctx, store.ListAlertsParams{Status: status, MaxRows: int32(limit)}) //nolint:gosec // bounded by the API
}

// Firing counts firing alerts (all, critical).
func (s *Service) Firing(ctx context.Context) (int64, int64, error) {
	c, err := store.New(s.db).CountFiringAlerts(ctx)
	return c.Total, c.Critical, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func bytesStr(b float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// addrOnly is the bare address of "Name <a@b>".
func addrOnly(s string) string {
	if a, err := mail.ParseAddress(s); err == nil {
		return a.Address
	}
	return s
}
