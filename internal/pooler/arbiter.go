package pooler

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/floatip"
	"github.com/israel-duff/xenibase/internal/store"
)

// HostView is what the arbiter knows about one pooler host in one check.
type HostView struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	ServerID   string    `json:"server_id"`
	Reachable  bool      `json:"reachable"`
	Ready      bool      `json:"ready"`
	Stale      bool      `json:"stale"`
	VRRP       string    `json:"vrrp_state"`
	Generation int64     `json:"generation"`
	Reason     string    `json:"reason,omitempty"`
	// AllowlistOff: a PgBouncer on the host ignores hba.conf, so the host
	// counts as not ready.
	AllowlistOff bool `json:"allowlist_off,omitempty"`
}

func (h HostView) healthy() bool { return h.Reachable && h.Ready }

// Decision is the arbiter's verdict on one check.
type Decision struct {
	// AssignTo is the host the floating IP should move to, if it must move.
	AssignTo *HostView
	// Follow: AssignTo is keepalived's MASTER, so the move is keepalived's
	// failover and happens without the grace checks.
	Follow bool
	// Holder is the host the floating IP routes to now (nil if none or an
	// unknown server).
	Holder *HostView
	// SplitBrain: more than one host says keepalived made it MASTER.
	SplitBrain bool
	// NoHealthy: no pooler host can serve.
	NoHealthy bool
}

// Decide is the arbiter's rule (V3 §2.1). The pooler hosts' agents no
// longer assign the floating IP themselves (that needs a Hetzner token on
// an internet-facing host), so the arbiter follows keepalived: when one
// healthy host is MASTER and the IP is elsewhere, it moves there at once
// (Follow). Otherwise the IP stays where it is while that host is healthy;
// with two MASTERs (split brain) it is not moved. When the holder is
// unhealthy, unknown, or the IP is unassigned, it goes to a healthy host,
// preferring the one keepalived made MASTER.
func Decide(hosts []HostView, holder string, manageIP bool) Decision {
	var d Decision
	masters, healthy := 0, 0
	var master *HostView
	for i := range hosts {
		h := &hosts[i]
		if h.Reachable && h.VRRP == "MASTER" {
			masters++
			master = h
		}
		if h.healthy() {
			healthy++
		}
		if holder != "" && h.ServerID == holder {
			d.Holder = h
		}
	}
	d.SplitBrain = masters > 1
	d.NoHealthy = len(hosts) > 0 && healthy == 0
	if manageIP && masters == 1 && master.healthy() && master.ServerID != "" && master.ServerID != holder {
		d.AssignTo, d.Follow = master, true
		return d
	}
	if !manageIP || (d.Holder != nil && d.Holder.healthy()) {
		return d
	}
	var pick *HostView
	for i := range hosts {
		h := &hosts[i]
		if !h.healthy() || h.ServerID == "" {
			continue
		}
		if pick == nil || (h.VRRP == "MASTER" && pick.VRRP != "MASTER") {
			pick = h
		}
	}
	d.AssignTo = pick
	return d
}

// Arbiter checks the pooler hosts and the floating IP every few seconds,
// keeps stale hosts caught up, and moves the IP when keepalived can't. Each
// region's pair has its own floating IP and is judged on its own (V3 §6.1).
type Arbiter struct {
	m      *Manager
	fip    floatip.Provider // the home region's
	fipFor func(region string) floatip.Provider
	log    *slog.Logger
	// Grace is how many consecutive checks the holder must be unhealthy
	// before the arbiter moves the IP itself (default 2), so keepalived's
	// own failover goes first.
	Grace int
	// RepushEvery limits re-pushes to stale hosts (default 15s).
	RepushEvery time.Duration

	mu       sync.Mutex
	states   map[string]*regionState
	wasReady map[uuid.UUID]bool
}

// regionState is what the arbiter remembers about one region's pair.
type regionState struct {
	snapshot   ArbiterSnapshot
	badChecks  int
	lastHolder string
	split      bool
	lastRepush time.Time
}

// ArbiterSnapshot is the arbiter's latest view, for the admin console.
type ArbiterSnapshot struct {
	Region     string     `json:"region"`
	CheckedAt  time.Time  `json:"checked_at"`
	Generation int64      `json:"generation"`
	Hosts      []HostView `json:"hosts"`
	// HolderServerID is the server the floating IP routes to ("" when
	// there is no floating IP or it is unassigned).
	HolderServerID string `json:"holder_server_id"`
	HolderName     string `json:"holder_name,omitempty"`
	HolderError    string `json:"holder_error,omitempty"`
	ManagesIP      bool   `json:"manages_ip"`
	SplitBrain     bool   `json:"split_brain"`
	NoHealthy      bool   `json:"no_healthy"`
}

// NewArbiter returns an arbiter for m's pooler hosts. fip is the home
// region's floating IP; nil means none is managed (keepalived alone moves
// the address).
func NewArbiter(m *Manager, fip floatip.Provider, log *slog.Logger) *Arbiter {
	if fip == nil {
		fip = floatip.None{}
	}
	return &Arbiter{m: m, fip: fip, log: log, Grace: 2, RepushEvery: 15 * time.Second, states: map[string]*regionState{}, wasReady: map[uuid.UUID]bool{}}
}

// SetRegionIPs supplies other regions' floating IPs (nil or floatip.None:
// that region's IP isn't managed).
func (a *Arbiter) SetRegionIPs(f func(region string) floatip.Provider) { a.fipFor = f }

func (a *Arbiter) provider(region string) (floatip.Provider, bool) {
	fip := a.fip
	if region != a.m.Home() {
		fip = nil
		if a.fipFor != nil {
			fip = a.fipFor(region)
		}
	}
	if fip == nil {
		return floatip.None{}, false
	}
	_, none := fip.(floatip.None)
	return fip, !none
}

func (a *Arbiter) state(region string) *regionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.states[region]
	if st == nil {
		st = &regionState{}
		a.states[region] = st
	}
	return st
}

// Snapshot returns the home region's latest check.
func (a *Arbiter) Snapshot() ArbiterSnapshot { return a.SnapshotFor(a.m.Home()) }

// SnapshotFor returns a region's latest check.
func (a *Arbiter) SnapshotFor(region string) ArbiterSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.states[region]
	if st == nil {
		return ArbiterSnapshot{Region: region}
	}
	s := st.snapshot
	s.Hosts = append([]HostView(nil), s.Hosts...)
	return s
}

// Regions lists the regions the arbiter has checked.
func (a *Arbiter) Regions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.states))
	for r := range a.states {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Run checks every interval until ctx ends.
func (a *Arbiter) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := a.Tick(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("pooler arbiter check failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs one check of every region's pair.
func (a *Arbiter) Tick(ctx context.Context) error {
	hs := a.m.hostSet()
	if hs == nil {
		return nil
	}
	entries, err := hs.refresh(ctx)
	if err != nil {
		return err
	}
	byRegion := map[string][]*poolerHostEntry{a.m.Home(): nil}
	for _, e := range entries {
		byRegion[e.node.Region] = append(byRegion[e.node.Region], e)
	}
	var errs []error
	for region, group := range byRegion {
		errs = append(errs, a.tickRegion(ctx, hs, region, group))
	}
	return errors.Join(errs...)
}

func (a *Arbiter) tickRegion(ctx context.Context, hs *hostSet, region string, entries []*poolerHostEntry) error {
	fip, manageIP := a.provider(region)
	rs := a.state(region)
	if len(entries) == 0 {
		a.mu.Lock()
		rs.snapshot = ArbiterSnapshot{Region: region, CheckedAt: time.Now(), ManagesIP: manageIP}
		a.mu.Unlock()
		return nil
	}
	q := store.New(a.m.db)
	cfg, err := q.GetPoolerGeneration(ctx, region)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	views := make([]HostView, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			st, err := hs.driver.Expect(cctx, e.node, agentapi.PoolerExpected{Generation: cfg.Generation, Hash: cfg.Hash})
			cancel()
			v := HostView{ID: e.node.ID, Name: e.node.Name}
			if e.node.ProviderServerID != nil {
				v.ServerID = *e.node.ProviderServerID
			}
			if err != nil {
				v.Reason = err.Error()
			} else {
				v.Reachable, v.Ready, v.Stale, v.VRRP, v.Generation, v.Reason = true, st.Ready, st.Stale, st.VRRPState, st.Generation, st.Reason
				// A host that ignores hba.conf would serve every project
				// past its IP allowlist: it must not hold the address.
				hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				why := hs.enforcement(hctx, e)
				cancel()
				if why != "" {
					v.Ready, v.AllowlistOff = false, true
					v.Reason = strings.TrimPrefix(v.Reason+"; "+why, "; ")
				}
				if st.ServerID != "" && st.ServerID != v.ServerID {
					v.ServerID = st.ServerID
					sid := st.ServerID
					if err := q.SetProviderServerID(ctx, store.SetProviderServerIDParams{ID: e.node.ID, ProviderServerID: &sid}); err != nil {
						a.log.Warn("could not record pooler host server ID", "host", e.node.Name, "err", err)
					}
				}
			}
			hs.setReachable(e.node.ID, err == nil, st)
			var vrrp *string
			if v.VRRP != "" {
				vrrp = &v.VRRP
			}
			gen, hash := v.Generation, st.Hash
			if err := q.RecordPoolerHostState(ctx, store.RecordPoolerHostStateParams{
				ID: e.node.ID, PoolerGeneration: &gen, PoolerHash: &hash, PoolerVrrpState: vrrp, PoolerReady: &v.Ready,
			}); err != nil {
				a.log.Warn("could not record pooler host state", "host", e.node.Name, "err", err)
			}
			views[i] = v
		}()
	}
	wg.Wait()

	// A reachable host serving an old generation missed a push: push
	// again (at most every RepushEvery).
	for _, v := range views {
		behind := v.Stale || v.Generation == 0 || v.Generation < cfg.Generation
		if v.Reachable && behind && time.Since(rs.lastRepush) >= a.RepushEvery {
			rs.lastRepush = time.Now()
			a.log.Info("pooler host is stale; pushing the configuration again", "host", v.Name, "serving", v.Generation, "expected", cfg.Generation)
			if err := a.m.Reload(ctx); err != nil {
				a.log.Warn("re-push to stale pooler host", "err", err)
			}
			break
		}
	}

	holder, herr := "", error(nil)
	if manageIP {
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		holder, herr = fip.Holder(hctx)
		cancel()
	}
	d := Decide(views, holder, manageIP && herr == nil)
	a.observe(ctx, rs, views, d, holder)

	if d.AssignTo != nil {
		a.mu.Lock()
		rs.badChecks++
		bad := rs.badChecks
		a.mu.Unlock()
		if d.Follow || bad >= a.Grace {
			to := *d.AssignTo
			from := "nobody"
			if d.Holder != nil {
				from = d.Holder.Name
			}
			actx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := fip.Assign(actx, to.ServerID)
			cancel()
			if err != nil {
				a.log.Error("could not move the floating IP", "to", to.Name, "err", err)
			} else {
				holder = to.ServerID
				a.mu.Lock()
				rs.badChecks = 0
				if !d.Follow {
					rs.lastHolder = holder
				}
				a.mu.Unlock()
				if d.Follow {
					// keepalived's failover: the next check records it as
					// took_ip, as when an agent assigned it.
					a.log.Info("moved the floating IP to keepalived's MASTER", "from", from, "to", to.Name)
				} else {
					a.log.Warn("moved the floating IP: keepalived did not", "from", from, "to", to.Name)
					a.m.event(ctx, &to.ID, "reassigned", map[string]any{"from": from, "to": to.Name, "by": "arbiter"})
				}
			}
		}
	} else {
		a.mu.Lock()
		rs.badChecks = 0
		a.mu.Unlock()
	}

	snap := ArbiterSnapshot{
		Region: region, CheckedAt: time.Now(), Generation: cfg.Generation, Hosts: views, HolderServerID: holder,
		ManagesIP: manageIP, SplitBrain: d.SplitBrain, NoHealthy: d.NoHealthy,
	}
	if herr != nil {
		snap.HolderError = herr.Error()
	}
	for _, v := range views {
		if holder != "" && v.ServerID == holder {
			snap.HolderName = v.Name
		}
	}
	a.mu.Lock()
	rs.snapshot = snap
	a.mu.Unlock()
	return nil
}

// observe records transitions as pooler events: the IP changing hands,
// split brain starting, hosts going stale or recovering.
func (a *Arbiter) observe(ctx context.Context, rs *regionState, views []HostView, d Decision, holder string) {
	a.mu.Lock()
	prevHolder, prevSplit := rs.lastHolder, rs.split
	rs.lastHolder, rs.split = holder, d.SplitBrain
	type change struct {
		v    HostView
		kind string
	}
	var changes []change
	for _, v := range views {
		was, seen := a.wasReady[v.ID]
		a.wasReady[v.ID] = v.healthy()
		switch {
		case seen && was && !v.healthy() && v.Stale:
			changes = append(changes, change{v, "stale"})
		case seen && !was && v.healthy():
			changes = append(changes, change{v, "recovered"})
		}
		if v.AllowlistOff && (!seen || was) {
			a.log.Error("pooler host does not enforce IP allowlists; it counts as not ready", "host", v.Name, "reason", v.Reason)
		}
	}
	a.mu.Unlock()

	if holder != "" && holder != prevHolder && d.Holder != nil {
		a.m.event(ctx, &d.Holder.ID, "took_ip", map[string]any{"server_id": holder, "vrrp_state": d.Holder.VRRP})
		a.log.Info("floating IP now routes to pooler host", "host", d.Holder.Name)
	}
	if d.SplitBrain && !prevSplit {
		a.m.event(ctx, nil, "split_brain", map[string]any{"holder": holder})
		a.log.Error("more than one pooler host is keepalived MASTER")
	}
	for _, c := range changes {
		a.m.event(ctx, &c.v.ID, c.kind, map[string]any{"reason": c.v.Reason, "generation": c.v.Generation})
	}
}
