package tlscert

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

// Regions keeps a certificate for each region with its own pooler
// hostname (regions.pooler_host), written in that region's directory
// (regions/<id>/), which its pooler hosts receive. Without it they got the
// home region's pair, which does not name their hostname. Each region has
// its own Manager with the home Manager's mode and settings. In files mode
// the operator's one pair is the home region's, and a region's poolers fall
// back to it, so it must name their hostnames too (a wildcard, or several
// names); Regions manages nothing then.
type Regions struct {
	base  Config
	dir   func(region string) string
	hosts func(ctx context.Context) (map[string]string, error)

	mu      sync.Mutex
	ctx     context.Context
	running map[string]*regionCert
}

type regionCert struct {
	m      *Manager
	cancel context.CancelFunc
	done   chan struct{}
}

// NewRegions returns the set. base is the home Manager's Config (its Dir
// is ignored), dir a region's pooler directory, and hosts the hostname of
// each region that has its own (the home region left out).
func NewRegions(base Config, dir func(region string) string, hosts func(ctx context.Context) (map[string]string, error)) *Regions {
	if base.Log == nil {
		base.Log = slog.Default()
	}
	return &Regions{base: base, dir: dir, hosts: hosts, running: map[string]*regionCert{}}
}

// Sync starts a Manager for every region that now has its own hostname,
// moves one whose hostname changed, and stops one that no longer has it.
// Managers started before Run wait for it.
func (r *Regions) Sync(ctx context.Context) error {
	if r.base.Mode == ModeOff || r.base.Mode == ModeFiles {
		return nil
	}
	hosts, err := r.hosts(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for region, rc := range r.running {
		if hosts[region] == "" {
			r.stop(region, rc)
		}
	}
	var errs []error
	for region, host := range hosts {
		if host == "" {
			continue
		}
		if rc, ok := r.running[region]; ok {
			if rc.m.Host() != host {
				rc.m.SetHost(host)
			}
			continue
		}
		cfg := r.base
		cfg.Dir = r.dir(region)
		if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
			errs = append(errs, fmt.Errorf("region %s: %w", region, err))
			continue
		}
		m, err := New(cfg, host)
		if err != nil {
			errs = append(errs, fmt.Errorf("region %s: %w", region, err))
			continue
		}
		// Something to serve until the first check.
		if err := m.Bootstrap(); err != nil {
			errs = append(errs, fmt.Errorf("region %s: %w", region, err))
			continue
		}
		rc := &regionCert{m: m}
		r.running[region] = rc
		r.base.Log.Info("pooler TLS for a region", "region", region, "host", host, "mode", cfg.Mode)
		if r.ctx != nil {
			r.start(rc)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("pooler TLS: %v", errs)
	}
	return nil
}

func (r *Regions) start(rc *regionCert) {
	ctx, cancel := context.WithCancel(r.ctx)
	rc.cancel, rc.done = cancel, make(chan struct{})
	go func() { defer close(rc.done); rc.m.Run(ctx) }()
}

func (r *Regions) stop(region string, rc *regionCert) {
	if rc.cancel != nil {
		rc.cancel()
		<-rc.done
	}
	delete(r.running, region)
	r.base.Log.Info("pooler TLS for a region stopped: it has no hostname of its own now", "region", region)
}

// Run keeps every region's certificate current, and the set in step with
// the regions every interval, until ctx ends.
func (r *Regions) Run(ctx context.Context, interval time.Duration) {
	r.mu.Lock()
	r.ctx = ctx
	for _, rc := range r.running {
		r.start(rc)
	}
	r.mu.Unlock()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			for region, rc := range r.running {
				r.stop(region, rc)
			}
			r.mu.Unlock()
			return
		case <-t.C:
			if err := r.Sync(ctx); err != nil && ctx.Err() == nil {
				r.base.Log.Warn("pooler TLS for regions", "err", err)
			}
		}
	}
}

// EnsureNow checks every region's certificate once (tests and startup).
func (r *Regions) EnsureNow(ctx context.Context) error {
	r.mu.Lock()
	ms := make([]*Manager, 0, len(r.running))
	for _, rc := range r.running {
		ms = append(ms, rc.m)
	}
	r.mu.Unlock()
	var errs []error
	for _, m := range ms {
		if err := m.EnsureNow(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("pooler TLS: %v", errs)
	}
	return nil
}

// Status is each region's certificate, by region.
func (r *Regions) Status() map[string]Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Status, len(r.running))
	for region, rc := range r.running {
		out[region] = rc.m.Status()
	}
	return out
}

// HTTPChallengeHandler answers the regions' ACME HTTP-01 challenges ahead
// of next. Caddy forwards /.well-known/acme-challenge/ for the regions'
// hostnames to xenibase-server as for the home one.
func (r *Regions) HTTPChallengeHandler(next http.Handler) http.Handler {
	if r.base.Mode != ModeACME {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		regions := make([]string, 0, len(r.running))
		for region := range r.running {
			regions = append(regions, region)
		}
		sort.Strings(regions)
		ms := make([]*Manager, 0, len(regions))
		for _, region := range regions {
			ms = append(ms, r.running[region].m)
		}
		r.mu.Unlock()
		for _, m := range ms {
			if m.handleChallenge(w, req) {
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}
