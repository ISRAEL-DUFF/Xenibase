package statuspage

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/statusapi"
)

// Service is a running status service.
type Service struct {
	cfg   *Config
	st    *store
	log   *slog.Logger
	known map[string]bool
	// Now and Probe are swapped in tests.
	Now   func() time.Time
	Probe func(ctx context.Context, p Probe) error

	limiter *rateLimiter
	tickMu  sync.Mutex
}

// New opens the status database.
func New(cfg *Config, log *slog.Logger) (*Service, error) {
	st, err := openStore(cfg.Data)
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, st: st, log: log, known: map[string]bool{}, Now: time.Now, Probe: runProbe, limiter: newRateLimiter(5, time.Hour)}
	for _, c := range cfg.Components {
		s.known[c.ID] = true
	}
	return s, nil
}

// Close closes the database.
func (s *Service) Close() error { return s.st.Close() }

// Run checks every interval and delivers mail until ctx ends.
func (s *Service) Run(ctx context.Context) {
	go s.runMailer(ctx)
	t := time.NewTicker(s.cfg.Interval.Duration)
	defer t.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("status check failed", "err", err)
		}
		if err := s.SLATick(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("SLA probes failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// runProbe is the real probe.
func runProbe(ctx context.Context, p Probe) error {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout.Duration)
	defer cancel()
	switch p.Kind {
	case "http":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "xenibase-status")
		resp, err := probeHTTP.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	case "postgres":
		c, err := pgx.Connect(ctx, p.DSN)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(context.Background()) }()
		rows, err := c.Query(ctx, p.Query)
		if err != nil {
			return err
		}
		for rows.Next() {
		}
		rows.Close()
		return rows.Err()
	case "tcp":
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", p.Addr)
		if err != nil {
			return err
		}
		return c.Close()
	}
	return fmt.Errorf("unknown probe kind %q", p.Kind)
}

var probeHTTP = &http.Client{
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DisableKeepAlives: true},
	CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

// probeResult is one probe's outcome in a check.
type probeResult struct {
	Name string
	Err  error
}

// combine is a probed component's state: down when every probe failed,
// degraded when some did.
func combine(results []probeResult) (string, string) {
	var failed []string
	for _, r := range results {
		if r.Err != nil {
			failed = append(failed, r.Name+": "+shortErr(r.Err))
		}
	}
	switch {
	case len(failed) == 0:
		return statusapi.Operational, ""
	case len(failed) == len(results):
		return statusapi.Down, strings.Join(failed, "; ")
	default:
		return statusapi.Degraded, strings.Join(failed, "; ")
	}
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// heartbeatState is a heartbeat component's state: what xenibase-server last
// said, or unknown once that is older than ttl.
func heartbeatState(h heartbeat, ok bool, now time.Time, ttl time.Duration) (string, string) {
	switch {
	case !ok:
		return statusapi.Unknown, "No report from Xenibase yet."
	case now.Sub(h.At) > ttl:
		return statusapi.Unknown, "No report from Xenibase since " + h.At.Format("2006-01-02 15:04 MST") + "."
	default:
		return h.State, h.Detail
	}
}

// action is what a check means for a component's automatic incident.
type action int

const (
	noAction action = iota
	openIncident
	resolveIncident
)

// advance applies one check to a component's track (V3 §2.6 incident
// rules): failAfter failing checks in a row open an incident,
// recoverAfter good ones resolve it, and unknown counts for neither.
func advance(t track, state, detail string, now time.Time, failAfter, recoverAfter int) (track, action) {
	if t.Since.IsZero() || t.State != state {
		t.Since = now
	}
	t.State, t.Detail, t.CheckedAt = state, detail, now
	switch state {
	case statusapi.Down, statusapi.Degraded:
		t.Fails++
		t.Oks = 0
		if t.AutoIncident == "" && t.Fails >= failAfter {
			return t, openIncident
		}
	case statusapi.Operational:
		t.Oks++
		t.Fails = 0
		if t.AutoIncident != "" && t.Oks >= recoverAfter {
			return t, resolveIncident
		}
	default:
		t.Fails, t.Oks = 0, 0
	}
	return t, noAction
}

// impact is how an open incident shows on its components.
func impact(severity string) string {
	if severity == "minor" || severity == "maintenance" {
		return statusapi.Degraded
	}
	return statusapi.Down
}

// displayState is what the page shows for a component: the worse of what
// was measured and what open incidents posted in Xenibase say.
func displayState(measured, id string, open []statusapi.Incident) string {
	r := statusapi.Rank(measured)
	for _, in := range open {
		if in.Auto || in.ResolvedAt != nil {
			continue
		}
		for _, c := range in.Components {
			if c == id {
				r = max(r, statusapi.Rank(impact(in.Severity)))
			}
		}
	}
	return statusapi.StateOf(r)
}

// overall summarises the page.
func overall(states []string) (string, string) {
	worst, unknown := 0, false
	for _, s := range states {
		worst = max(worst, statusapi.Rank(s))
		unknown = unknown || s == statusapi.Unknown
	}
	switch {
	case worst == statusapi.Rank(statusapi.Down):
		return statusapi.Down, "Major outage"
	case worst == statusapi.Rank(statusapi.Degraded):
		return statusapi.Degraded, "Partial outage"
	case unknown:
		return statusapi.Unknown, "Some components have no recent data"
	default:
		return statusapi.Operational, "All systems operational"
	}
}

// Tick runs one check of every component.
func (s *Service) Tick(ctx context.Context) error {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	now := s.Now().UTC()
	hbs, err := s.st.heartbeats(ctx)
	if err != nil {
		return err
	}
	type measured struct{ state, detail string }
	m := make([]measured, len(s.cfg.Components))
	var wg sync.WaitGroup
	for i, c := range s.cfg.Components {
		if len(c.Probes) == 0 {
			h, ok := hbs[c.ID]
			st, d := heartbeatState(h, ok, now, s.cfg.HeartbeatTTL.Duration)
			m[i] = measured{st, d}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := make([]probeResult, len(c.Probes))
			var pwg sync.WaitGroup
			for j, p := range c.Probes {
				pwg.Add(1)
				go func() {
					defer pwg.Done()
					res[j] = probeResult{Name: p.Name, Err: s.Probe(ctx, p)}
				}()
			}
			pwg.Wait()
			st, d := combine(res)
			if c.Heartbeat {
				// Both: the worse wins, with both details.
				h, ok := hbs[c.ID]
				hst, hd := heartbeatState(h, ok, now, s.cfg.HeartbeatTTL.Duration)
				if statusapi.Rank(hst) > statusapi.Rank(st) {
					st = hst
				}
				d = strings.Trim(strings.Join([]string{hd, d}, " "), " ")
				if st == statusapi.Operational {
					d = ""
				}
			}
			m[i] = measured{st, d}
		}()
	}
	wg.Wait()

	tracks, err := s.st.tracks(ctx)
	if err != nil {
		return err
	}
	tx, err := s.st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	minute := now.Truncate(time.Minute)
	type notice struct {
		in statusapi.Incident
		u  []statusapi.IncidentUpdate
	}
	var notices []notice
	for i, c := range s.cfg.Components {
		prev := tracks[c.ID]
		t, act := advance(prev, m[i].state, m[i].detail, now, s.cfg.FailAfter, s.cfg.RecoverAfter)
		if prev.State != "" && prev.State != t.State {
			s.log.Info("component state changed", "component", c.ID, "from", prev.State, "to", t.State, "detail", t.Detail)
		}
		switch act {
		case openIncident:
			in := s.autoIncident(c, t, now)
			fresh, err := s.st.putIncident(ctx, tx, in, now)
			if err != nil {
				return err
			}
			t.AutoIncident = in.ID
			notices = append(notices, notice{in, fresh})
			s.log.Warn("opened an incident", "component", c.ID, "incident", in.ID, "detail", t.Detail)
		case resolveIncident:
			in, err := s.st.incident(ctx, tx, t.AutoIncident)
			if err != nil {
				return err
			}
			t.AutoIncident = ""
			if in != nil && in.ResolvedAt == nil {
				in.Status, in.ResolvedAt = "resolved", &now
				in.Updates = append(in.Updates, statusapi.IncidentUpdate{
					ID: "resolved", Status: "resolved", PostedAt: now,
					Body: "Checks from outside Xenibase are passing again. This incident was resolved automatically.",
				})
				fresh, err := s.st.putIncident(ctx, tx, *in, now)
				if err != nil {
					return err
				}
				notices = append(notices, notice{*in, fresh})
				s.log.Info("resolved an incident", "component", c.ID, "incident", in.ID)
			}
		}
		if err := s.st.saveTrack(ctx, tx, c.ID, t); err != nil {
			return err
		}
		if err := recordMinute(ctx, tx, c.ID, minute, t.State); err != nil {
			return err
		}
	}
	for _, n := range notices {
		for _, u := range n.u {
			if err := s.notify(ctx, tx, n.in, u, now); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM subscribers WHERE confirmed_at IS NULL AND created_at < ?`, now.Add(-7*24*time.Hour).Unix()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.st.rollUp(ctx, now.Add(-24*time.Hour), now)
}

func (s *Service) autoIncident(c Component, t track, now time.Time) statusapi.Incident {
	title, sev := c.Name+" is unavailable", "major"
	if t.State == statusapi.Degraded {
		title, sev = c.Name+" is degraded", "minor"
	}
	started := now.Add(-time.Duration(t.Fails-1) * s.cfg.Interval.Duration)
	return statusapi.Incident{
		ID: fmt.Sprintf("auto-%s-%d", c.ID, now.Unix()), Title: title, Components: []string{c.ID}, Region: c.Region,
		Severity: sev, Status: "investigating", StartedAt: started, Auto: true,
		Updates: []statusapi.IncidentUpdate{{
			ID: "opened", Status: "investigating", PostedAt: now,
			Body: fmt.Sprintf("Checks from outside Xenibase have failed %d times in a row (%s). We are investigating.", t.Fails, t.Detail),
		}},
	}
}

// PutIncident stores an incident pushed by xenibase-server and notifies
// subscribers of its new updates.
func (s *Service) PutIncident(ctx context.Context, in statusapi.Incident) error {
	in.Auto = false
	if err := in.Validate(); err != nil {
		return err
	}
	now := s.Now().UTC()
	tx, err := s.st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	fresh, err := s.st.putIncident(ctx, tx, in, now)
	if err != nil {
		return err
	}
	for _, u := range fresh {
		if err := s.notify(ctx, tx, in, u, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Heartbeat stores xenibase-server's report, unless it was sent before the
// newest one accepted (errStaleHeartbeat).
func (s *Service) Heartbeat(ctx context.Context, hb statusapi.Heartbeat) error {
	if err := hb.Validate(); err != nil {
		return err
	}
	return s.st.saveHeartbeat(ctx, s.heartbeatStates(hb), hb.SentAt, s.Now().UTC())
}

// heartbeatStates maps a heartbeat's states to the page's components: a
// state for a region applies to that region's component of its ID, and
// one without a region to every component of its ID that has no state
// of its own region in the same heartbeat.
func (s *Service) heartbeatStates(hb statusapi.Heartbeat) map[string]statusapi.ComponentState {
	out := map[string]statusapi.ComponentState{}
	for _, c := range s.cfg.Components {
		if !c.Heartbeat {
			continue
		}
		var all, own *statusapi.ComponentState
		for i, st := range hb.Components {
			switch {
			case st.ID != c.HeartbeatID:
			case st.Region == "":
				all = &hb.Components[i]
			case st.Region == c.Region:
				own = &hb.Components[i]
			}
		}
		if own != nil {
			out[c.ID] = *own
		} else if all != nil {
			out[c.ID] = *all
		}
	}
	return out
}
