package config

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Insight configures the SQL console and metrics (M6).
type Insight struct {
	// ConsoleDisabled turns the SQL console and table browser off for every
	// project (XENIBASE_CONSOLE_DISABLED).
	ConsoleDisabled bool
	// MetricsInterval is how often project and node metrics are sampled
	// (XENIBASE_METRICS_INTERVAL, default 1m).
	MetricsInterval time.Duration
	// MetricsToken lets a scraper read /metrics with "Authorization:
	// Bearer <token>" (XENIBASE_METRICS_TOKEN). Without it only signed-in
	// operators can.
	MetricsToken string
	// AlertsInterval is how often alert conditions are evaluated
	// (XENIBASE_ALERTS_INTERVAL, default 30s).
	AlertsInterval time.Duration
	// PublicURL is the web UI's address, for links in alerts
	// (XENIBASE_PUBLIC_URL, e.g. https://xenibase.example.com).
	PublicURL string
	// TenancySweep is how often opaque renames, credential grace periods,
	// usage recording, and org deletions are processed
	// (XENIBASE_TENANCY_SWEEP_INTERVAL, default 5m).
	TenancySweep time.Duration
	// OutboundBlock are extra address ranges webhooks and HTTP jobs may
	// never reach, such as the nodes' network (XENIBASE_OUTBOUND_BLOCK,
	// comma-separated CIDRs), besides the private, loopback, link-local and
	// metadata ranges always refused (V2 §9.1).
	OutboundBlock []netip.Prefix
	// QueryInsightsInterval is how often pg_stat_statements is snapshotted
	// (XENIBASE_INSIGHTS_INTERVAL, default 5m, V3 §8).
	QueryInsightsInterval time.Duration
	// SlowQuery is the slow-query log's threshold (XENIBASE_SLOW_QUERY_MS,
	// default 1000).
	SlowQuery time.Duration
	// InsightsPlans are the plans whose shared projects get query insights
	// (XENIBASE_INSIGHTS_PLANS, default "pro,team"; "all" for every plan, for
	// installations without billing). Dedicated projects always do.
	InsightsPlans []string
	// WebhookListenMax caps the LISTEN connections the webhook deliverer
	// holds to one instance (XENIBASE_WEBHOOK_LISTEN_MAX, default 50; 0
	// for none, so every project is polled). They are direct connections,
	// outside the poolers, counted against the instance's max_connections.
	WebhookListenMax int
	// WebhookIdleAfter is how long a project's outbox has nothing to send
	// before its LISTEN connection is released (XENIBASE_WEBHOOK_IDLE_AFTER,
	// default 10m).
	WebhookIdleAfter time.Duration
	// WebhookIdlePoll is how often the outbox of a project without a LISTEN
	// connection is read (XENIBASE_WEBHOOK_IDLE_POLL, default 30s).
	WebhookIdlePoll time.Duration
}

func loadInsight(getenv func(string) string, cfg *Config) []error {
	var errs []error
	in := Insight{MetricsInterval: time.Minute, MetricsToken: getenv("XENIBASE_METRICS_TOKEN"), AlertsInterval: 30 * time.Second,
		PublicURL: strings.TrimRight(getenv("XENIBASE_PUBLIC_URL"), "/")}
	if v := getenv("XENIBASE_ALERTS_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > time.Hour {
			errs = append(errs, fmt.Errorf("XENIBASE_ALERTS_INTERVAL: must be a duration from 1s to 1h, got %q", v))
		}
		in.AlertsInterval = d
	}
	if u := in.PublicURL; u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		errs = append(errs, fmt.Errorf("XENIBASE_PUBLIC_URL: must start with https://, got %q", u))
	}
	if v := getenv("XENIBASE_CONSOLE_DISABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("XENIBASE_CONSOLE_DISABLED: %w", err))
		}
		in.ConsoleDisabled = b
	}
	if v := getenv("XENIBASE_METRICS_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > time.Hour {
			errs = append(errs, fmt.Errorf("XENIBASE_METRICS_INTERVAL: must be a duration from 1s to 1h, got %q", v))
		}
		in.MetricsInterval = d
	}
	in.TenancySweep = 5 * time.Minute
	if v := getenv("XENIBASE_TENANCY_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > time.Hour {
			errs = append(errs, fmt.Errorf("XENIBASE_TENANCY_SWEEP_INTERVAL: must be a duration from 1s to 1h, got %q", v))
		}
		in.TenancySweep = d
	}
	for _, c := range strings.Split(getenv("XENIBASE_OUTBOUND_BLOCK"), ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		pfx, err := netip.ParsePrefix(c)
		if err != nil {
			errs = append(errs, fmt.Errorf("XENIBASE_OUTBOUND_BLOCK: %q is not a CIDR", c))
			continue
		}
		in.OutboundBlock = append(in.OutboundBlock, pfx)
	}
	in.QueryInsightsInterval = 5 * time.Minute
	if v := getenv("XENIBASE_INSIGHTS_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > time.Hour {
			errs = append(errs, fmt.Errorf("XENIBASE_INSIGHTS_INTERVAL: must be a duration from 1s to 1h, got %q", v))
		}
		in.QueryInsightsInterval = d
	}
	in.SlowQuery = time.Second
	if v := getenv("XENIBASE_SLOW_QUERY_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 10 || n > 3_600_000 {
			errs = append(errs, fmt.Errorf("XENIBASE_SLOW_QUERY_MS: must be 10 to 3600000, got %q", v))
		}
		in.SlowQuery = time.Duration(n) * time.Millisecond
	}
	in.InsightsPlans = []string{"pro", "team"}
	if v := strings.TrimSpace(getenv("XENIBASE_INSIGHTS_PLANS")); v != "" {
		in.InsightsPlans = nil
		for _, pl := range strings.Split(v, ",") {
			if pl = strings.TrimSpace(strings.ToLower(pl)); pl != "" {
				in.InsightsPlans = append(in.InsightsPlans, pl)
			}
		}
	}
	in.WebhookListenMax, in.WebhookIdleAfter, in.WebhookIdlePoll = 50, 10*time.Minute, 30*time.Second
	if v := getenv("XENIBASE_WEBHOOK_LISTEN_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 10_000 {
			errs = append(errs, fmt.Errorf("XENIBASE_WEBHOOK_LISTEN_MAX: must be 0 to 10000, got %q", v))
		}
		in.WebhookListenMax = n
	}
	if v := getenv("XENIBASE_WEBHOOK_IDLE_AFTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Second || d > 24*time.Hour {
			errs = append(errs, fmt.Errorf("XENIBASE_WEBHOOK_IDLE_AFTER: must be a duration from 10s to 24h, got %q", v))
		}
		in.WebhookIdleAfter = d
	}
	if v := getenv("XENIBASE_WEBHOOK_IDLE_POLL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second || d > 10*time.Minute {
			errs = append(errs, fmt.Errorf("XENIBASE_WEBHOOK_IDLE_POLL: must be a duration from 1s to 10m, got %q", v))
		}
		in.WebhookIdlePoll = d
	}
	if t := in.MetricsToken; t != "" && len(t) < 24 {
		errs = append(errs, errors.New("XENIBASE_METRICS_TOKEN: must be at least 24 characters"))
	}
	cfg.Insight = in
	return errs
}
