// Package capacity adds and removes database nodes by itself (V3 §5.2,
// §5.3): it watches each region's shared disk and dedicated headroom,
// proposes a server when a threshold trips, provisions proposals within
// the monthly infrastructure budget (others wait for the platform admin),
// drains nodes and rebalances projects with zero-downtime moves, and
// deletes servers that have stayed empty.
package capacity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/cloud"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/store"
)

// Errors.
var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Converter turns an amount into another currency at the current rate
// (package costs). Without it, a budget in another currency than a
// proposal's can't be checked and the proposal waits for the admin.
type Converter interface {
	Convert(ctx context.Context, minor int64, from, to string) (int64, error)
}

// Config is the installation's provisioning setup.
type Config struct {
	// Region is where this installation's nodes are (config.Cloud.Region).
	Region string
	// Location is the provider's location new servers go to (fsn1).
	Location string
	// Image, Network, PlacementGroup and SSHKeys are passed to the provider.
	Image          string
	Network        string
	PlacementGroup string
	SSHKeys        []string
	// Bootstrap is the cloud-init template's fixed part; NodeName and Token
	// are filled per server.
	Bootstrap cloud.Bootstrap
	// Edge is what an edge node runs besides its agent (V4.1 §11): nil
	// leaves edge proposals to be provisioned by hand.
	Edge *cloud.EdgeBootstrap
	// RegionAPIDomain is a region's own API domain, if it has one, which
	// an edge node there serves instead of Edge.Domain.
	RegionAPIDomain func(region string) string
	// JoinTimeout bounds the wait for a new server's agent (20 minutes).
	JoinTimeout time.Duration
	// Poll is how often the provisioning flow checks for the agent (5 s).
	Poll time.Duration
	Now  func() time.Time
}

// Service runs capacity automation.
type Service struct {
	db       *pgxpool.Pool
	nodes    *nodes.Service
	ded      *dedicated.Service
	provider cloud.Provider
	fx       Converter
	cfg      Config
	log      *slog.Logger

	mu      sync.Mutex
	catalog []cloud.ServerPrice
	catAt   time.Time
}

// New returns the service. provider is the cloud provider (a
// cloud.ManualProvider when servers are registered by hand).
func New(db *pgxpool.Pool, ns *nodes.Service, ded *dedicated.Service, provider cloud.Provider, fx Converter, cfg Config, log *slog.Logger) *Service {
	if cfg.Region == "" {
		cfg.Region = "eu-central"
	}
	if cfg.JoinTimeout <= 0 {
		cfg.JoinTimeout = 20 * time.Minute
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if provider == nil {
		provider = cloud.ManualProvider{}
	}
	return &Service{db: db, nodes: ns, ded: ded, provider: provider, fx: fx, cfg: cfg, log: log}
}

// SetConverter sets the exchange-rate converter budgets are checked with.
func (s *Service) SetConverter(fx Converter) { s.fx = fx }

// Provider is the provider servers are created with.
func (s *Service) Provider() cloud.Provider { return s.provider }

// Region is the installation's region.
func (s *Service) Region() string { return s.cfg.Region }

// Kinds returns the operation kinds this service runs.
func (s *Service) Kinds() map[string]jobs.Kind {
	return map[string]jobs.Kind{
		KindProvision: {Handler: s.runProvision, OnFail: s.failProvision, MaxAttempts: 2, Timeout: time.Hour},
	}
}

// ---- Settings ----------------------------------------------------------------

const settingsKey = "capacity"

// TierSettings are one tier's thresholds and the server it adds.
type TierSettings struct {
	// Enabled lets the planner propose servers for the tier.
	Enabled bool `json:"enabled"`
	// DiskThreshold (shared): propose when the region's shared disk is
	// projected past this fraction within HorizonDays.
	DiskThreshold float64 `json:"disk_threshold,omitempty"`
	HorizonDays   int     `json:"horizon_days,omitempty"`
	// ServerType is the provider's type to add ("cpx31"); empty picks the
	// cheapest that fits MinCPUs, MinMemoryGB and MinDiskGB.
	ServerType  string  `json:"server_type,omitempty"`
	MinCPUs     int     `json:"min_cpus,omitempty"`
	MinMemoryGB float64 `json:"min_memory_gb,omitempty"`
	MinDiskGB   int     `json:"min_disk_gb,omitempty"`
	// ClusterMemoryMB (shared) is the new node's shared cluster's memory.
	ClusterMemoryMB int `json:"cluster_memory_mb,omitempty"`
	// CPUThreshold (edge): propose an edge node when a region's edge
	// processes average above this percentage of their hosts' CPUs for an
	// hour (V4.1 §11).
	CPUThreshold float64 `json:"cpu_threshold,omitempty"`
}

// Settings are the platform's capacity settings (Admin → Capacity).
type Settings struct {
	// AutoApply provisions proposals within the budget by themselves.
	AutoApply bool `json:"auto_apply"`
	// MonthlyBudgetMinor is the monthly infrastructure budget, in minor
	// units of BudgetCurrency: the nodes' monthly cost plus a proposal's
	// must stay within it for the proposal to apply by itself.
	MonthlyBudgetMinor int64        `json:"monthly_budget_minor"`
	BudgetCurrency     string       `json:"budget_currency"`
	Shared             TierSettings `json:"shared"`
	Dedicated          TierSettings `json:"dedicated"`
	// Edge nodes run xenibase-edge only (V4.1 §11).
	Edge TierSettings `json:"edge"`
	// AutoRebalance moves a rebalance batch in the maintenance window
	// without waiting for approval.
	AutoRebalance bool `json:"auto_rebalance"`
	// RebalanceSpread: the rebalancer proposes moves when shared nodes'
	// disk use differs by more than this fraction (0.15).
	RebalanceSpread float64 `json:"rebalance_spread"`
	// DeleteEmptyAfterHours: an empty node from a provider is deleted after
	// this long (24).
	DeleteEmptyAfterHours int `json:"delete_empty_after_hours"`
}

// DefaultSettings: proposals apply by themselves within a budget of zero,
// so none does until the admin sets one.
func DefaultSettings() Settings {
	return Settings{
		AutoApply: true, BudgetCurrency: "EUR",
		Shared:          TierSettings{Enabled: true, DiskThreshold: 0.7, HorizonDays: 14, MinCPUs: 4, MinMemoryGB: 8, MinDiskGB: 160, ClusterMemoryMB: 2048},
		Dedicated:       TierSettings{Enabled: true, MinCPUs: 8, MinMemoryGB: 16, MinDiskGB: 240},
		Edge:            TierSettings{Enabled: true, CPUThreshold: 70, MinCPUs: 4, MinMemoryGB: 8, MinDiskGB: 40},
		RebalanceSpread: 0.15, DeleteEmptyAfterHours: 24,
	}
}

// Validate checks the settings.
func (st Settings) Validate() error {
	switch {
	case st.MonthlyBudgetMinor < 0:
		return invalid("the budget can't be negative")
	case len(st.BudgetCurrency) != 3:
		return invalid("the budget currency is a three-letter code such as EUR")
	case st.Shared.DiskThreshold <= 0 || st.Shared.DiskThreshold >= 1:
		return invalid("the shared disk threshold is a fraction between 0 and 1 (0.7)")
	case st.Shared.HorizonDays < 1 || st.Shared.HorizonDays > 90:
		return invalid("the horizon is 1 to 90 days")
	case st.Shared.ClusterMemoryMB < 512:
		return invalid("a shared cluster needs at least 512 MB")
	case st.RebalanceSpread <= 0 || st.RebalanceSpread >= 1:
		return invalid("the rebalance spread is a fraction between 0 and 1 (0.15)")
	case st.DeleteEmptyAfterHours < 1:
		return invalid("empty nodes wait at least an hour")
	case st.Edge.CPUThreshold <= 0 || st.Edge.CPUThreshold >= 100:
		return invalid("the edge CPU threshold is a percentage between 0 and 100 (70)")
	}
	return nil
}

// Settings returns the capacity settings (the defaults if none are saved).
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	out := DefaultSettings()
	raw, err := store.New(s.db).GetSetting(ctx, settingsKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("capacity settings: %w", err)
	}
	return out, nil
}

// SetSettings saves the capacity settings.
func (s *Service) SetSettings(ctx context.Context, st Settings) error {
	if err := st.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return store.New(s.db).PutSetting(ctx, store.PutSettingParams{Key: settingsKey, Value: raw})
}

// Catalog is the provider's price catalog, cached for an hour.
func (s *Service) Catalog(ctx context.Context) ([]cloud.ServerPrice, error) {
	s.mu.Lock()
	if s.catalog != nil && time.Since(s.catAt) < time.Hour {
		c := s.catalog
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()
	c, err := s.provider.PriceCatalog(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.catalog, s.catAt = c, time.Now()
	s.mu.Unlock()
	return c, nil
}

// Run evaluates capacity hourly, moves drains and rebalances along, and
// cleans up empty nodes, until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var last, lastRebalance time.Time
	for {
		if time.Since(last) >= time.Hour {
			if _, err := s.Evaluate(ctx); err != nil {
				s.log.Warn("capacity evaluation", "err", err)
			}
			last = time.Now()
		}
		if time.Since(lastRebalance) >= 7*24*time.Hour {
			if _, _, err := s.Rebalance(ctx); err != nil {
				s.log.Warn("rebalance", "err", err)
			}
			lastRebalance = time.Now()
		}
		if err := s.Step(ctx); err != nil {
			s.log.Warn("capacity step", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Step advances drains and rebalancing and sweeps empty nodes once.
func (s *Service) Step(ctx context.Context) error {
	return errors.Join(s.advanceMoves(ctx), s.sweepEmpty(ctx))
}
