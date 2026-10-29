// Package tenancy implements V2's multi-tenant hardening (§10): quotas and
// per-org rate limits, storage enforcement, the statement reaper, usage
// recording, suspension, break-glass, dedicated allowances, and org
// deletion. Its background loops run in every xenibase-server; each step is
// idempotent, so several servers converge on the same state.
package tenancy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

// Config tunes the background work. Zero values take the spec defaults.
type Config struct {
	// Now is the clock (tests move it).
	Now func() time.Time
	// ReaperInterval is how often the reaper runs (15 s).
	ReaperInterval time.Duration
	// StatementLimit is the longest a tenant statement may run on a shared
	// cluster (10 min); IdleTxLimit the longest a session may sit idle in
	// a transaction (5 min).
	StatementLimit time.Duration
	IdleTxLimit    time.Duration
	// EnforceInterval is how often storage limits are checked (1 min; the
	// sizes behind them are sampled every 5).
	EnforceInterval time.Duration
	// SweepInterval runs the slower housekeeping: opaque renames, credential
	// grace periods, usage recording, org deletion (5 min).
	SweepInterval time.Duration
	// PublicURL is the web UI's address, for links in emails.
	PublicURL string
}

func (c *Config) defaults() {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.ReaperInterval == 0 {
		c.ReaperInterval = 15 * time.Second
	}
	if c.StatementLimit == 0 {
		c.StatementLimit = 10 * time.Minute
	}
	if c.IdleTxLimit == 0 {
		c.IdleTxLimit = 5 * time.Minute
	}
	if c.EnforceInterval == 0 {
		c.EnforceInterval = time.Minute
	}
	if c.SweepInterval == 0 {
		c.SweepInterval = 5 * time.Minute
	}
}

// Service is the tenancy controller.
type Service struct {
	db       *pgxpool.Pool
	projects *provision.Service
	mail     *mail.Service
	cfg      Config
	log      *slog.Logger

	// FinalBackup takes a project's final backup (suspension, org deletion).
	FinalBackup func(ctx context.Context, p store.Project) error
	// BeforeStorageRecord, when set, runs after a storage lock change took
	// effect and before it is recorded; an error stops there, as a crash
	// would (failure-injection tests).
	BeforeStorageRecord func(ctx context.Context, projectID uuid.UUID) error

	consoleMu sync.Mutex
	console   map[uuid.UUID]int // running console queries per org

	trafficMu sync.Mutex
	traffic   map[string]int64 // last SHOW STATS byte counts per pooler/database

	softMu   sync.Mutex
	softBase map[uuid.UUID]float64 // ratio when first seen soft-locked (grewWhileSoft)
}

// New returns a Service and installs its login gate on projects.
func New(db *pgxpool.Pool, projects *provision.Service, m *mail.Service, cfg Config, log *slog.Logger) *Service {
	cfg.defaults()
	s := &Service{db: db, projects: projects, mail: m, cfg: cfg, log: log, console: map[uuid.UUID]int{}, softBase: map[uuid.UUID]float64{}}
	if projects != nil {
		projects.LoginGate = s.LoginAllowed
	}
	return s
}

// Now is the service's clock.
func (s *Service) Now() time.Time { return s.cfg.Now() }

// Run runs the background loops until ctx ends.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	loop := func(every time.Duration, name string, f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				if err := f(ctx); err != nil && ctx.Err() == nil {
					s.log.Warn(name, "err", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}
	loop(s.cfg.ReaperInterval, "reaper", func(ctx context.Context) error { _, err := s.Reap(ctx); return err })
	loop(s.cfg.EnforceInterval, "storage enforcement", s.EnforceStorage)
	loop(s.cfg.SweepInterval, "tenancy sweep", s.Sweep)
	wg.Wait()
}

// Sweep runs the slow housekeeping once.
func (s *Service) Sweep(ctx context.Context) error {
	var errs []error
	if _, err := s.projects.ScheduleOpaqueRenames(ctx, s.Now()); err != nil {
		errs = append(errs, fmt.Errorf("opaque renames: %w", err))
	}
	if err := s.RecordUsage(ctx); err != nil {
		errs = append(errs, fmt.Errorf("usage: %w", err))
	}
	if err := s.EndExpiredBreakGlass(ctx); err != nil {
		errs = append(errs, fmt.Errorf("break-glass: %w", err))
	}
	if err := s.FinishOrgDeletions(ctx); err != nil {
		errs = append(errs, fmt.Errorf("org deletion: %w", err))
	}
	if err := s.PruneRetention(ctx); err != nil {
		errs = append(errs, fmt.Errorf("retention: %w", err))
	}
	return errors.Join(errs...)
}

// Limits returns an organisation's effective quotas and plan.
func (s *Service) Limits(ctx context.Context, orgID uuid.UUID) (store.Limits, store.OrgWithPlanRow, error) {
	o, err := store.New(s.db).OrgWithPlan(ctx, orgID)
	if err != nil {
		return nil, o, err
	}
	l, err := store.EffectiveLimits(o.PlanLimits, o.LimitOverrides)
	return l, o, err
}

// LoginAllowed is provision's login gate: a project's logins work unless
// its storage is hard-locked or its organisation is suspended.
func (s *Service) LoginAllowed(ctx context.Context, p store.Project) (bool, error) {
	if p.OrgID == uuid.Nil {
		return true, nil // the isolation check's throwaway tenants
	}
	if p.StorageState == StateHard {
		return false, nil
	}
	o, err := store.New(s.db).GetOrg(ctx, p.OrgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return o.Status == OrgActive, nil
}

// Organisation statuses.
const (
	OrgActive    = "active"
	OrgSuspended = "suspended"
	OrgDeleting  = "deleting"
)

// notify emails addrs, logging rather than failing: a missing email must
// not stop enforcement.
func (s *Service) notify(ctx context.Context, addrs []string, subject, body string) {
	if s.mail == nil || len(addrs) == 0 {
		return
	}
	if err := s.mail.Send(ctx, mail.Message{To: addrs, Subject: subject, Body: body}); err != nil {
		s.log.Warn("send email", "subject", subject, "err", err)
	}
}

func (s *Service) link(path string) string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + path
}
