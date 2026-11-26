package pooler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/store"
)

// HostDriver reaches pooler hosts' agents (V3 §2.1). The nodes service
// implements it.
type HostDriver interface {
	// Hosts lists the registered pooler hosts.
	Hosts(ctx context.Context) ([]store.Node, error)
	// Push sends a configuration bundle to one host.
	Push(ctx context.Context, n store.Node, b agentapi.PoolerBundle) (agentapi.PoolerStatus, error)
	// Expect tells one host the newest generation, returning its status.
	Expect(ctx context.Context, n store.Node, e agentapi.PoolerExpected) (agentapi.PoolerStatus, error)
}

// HostAdminConfig is how xenibase-server reaches a pooler host's PgBouncer
// admin consoles: the host's private address and these ports.
type HostAdminConfig struct {
	User, Password, SSLMode string
	SessionPort, PooledPort int
}

// poolerHostEntry is a pooler host as the manager knows it.
type poolerHostEntry struct {
	node      store.Node
	session   *Admin
	pooled    *Admin
	reachable bool
	status    agentapi.PoolerStatus
	// The last check that both PgBouncers apply hba.conf: when, and why
	// not ("" when they do).
	hbaCheckedAt time.Time
	hbaProblem   string
}

// hbaCheckEvery spaces the arbiter's auth_type checks of a host.
const hbaCheckEvery = time.Minute

// enforcement says why e's PgBouncers do not apply hba.conf, or "" when
// they do (or can't be asked: the console being down is reported on its
// own). The answer is kept for hbaCheckEvery.
func (h *hostSet) enforcement(ctx context.Context, e *poolerHostEntry) string {
	h.mu.Lock()
	if !e.hbaCheckedAt.IsZero() && time.Since(e.hbaCheckedAt) < hbaCheckEvery {
		why := e.hbaProblem
		h.mu.Unlock()
		return why
	}
	session, pooled := e.session, e.pooled
	h.mu.Unlock()
	var why []string
	asked := false
	for _, a := range []*Admin{session, pooled} {
		if a == nil {
			continue
		}
		w, err := a.EnforcesHBA(ctx)
		if err != nil {
			continue
		}
		asked = true
		if w != "" {
			why = append(why, w)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if asked {
		e.hbaCheckedAt, e.hbaProblem = time.Now(), strings.Join(why, "; ")
	}
	return e.hbaProblem
}

type hostSet struct {
	driver HostDriver
	admin  HostAdminConfig

	mu      sync.Mutex
	entries map[uuid.UUID]*poolerHostEntry
	order   []uuid.UUID
}

// SetHostDriver makes the manager push its configuration to pooler hosts
// and send PgBouncer commands to their admin consoles, as well as to the
// local poolers it was built with (if any).
func (m *Manager) SetHostDriver(d HostDriver, admin HostAdminConfig) {
	if admin.SessionPort == 0 {
		admin.SessionPort = 5432
	}
	if admin.PooledPort == 0 {
		admin.PooledPort = 6543
	}
	m.hostsMu.Lock()
	m.hosts = &hostSet{driver: d, admin: admin, entries: map[uuid.UUID]*poolerHostEntry{}}
	m.hostsMu.Unlock()
}

func (m *Manager) hostSet() *hostSet {
	m.hostsMu.Lock()
	defer m.hostsMu.Unlock()
	return m.hosts
}

// refresh reloads the host list, keeping what is known about each host.
func (h *hostSet) refresh(ctx context.Context) ([]*poolerHostEntry, error) {
	nodes, err := h.driver.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[uuid.UUID]bool{}
	h.order = h.order[:0]
	var out []*poolerHostEntry
	for _, n := range nodes {
		seen[n.ID] = true
		e, ok := h.entries[n.ID]
		if !ok || e.node.PrivateAddr != n.PrivateAddr {
			s, err := NewAdmin("session@"+n.Name, net.JoinHostPort(n.PrivateAddr, strconv.Itoa(h.admin.SessionPort)), h.admin.User, h.admin.Password, h.admin.SSLMode)
			if err != nil {
				return nil, err
			}
			p, err := NewAdmin("transaction@"+n.Name, net.JoinHostPort(n.PrivateAddr, strconv.Itoa(h.admin.PooledPort)), h.admin.User, h.admin.Password, h.admin.SSLMode)
			if err != nil {
				return nil, err
			}
			// A new host is assumed reachable until a call says otherwise.
			reach := true
			if ok {
				reach = e.reachable
			}
			e = &poolerHostEntry{session: s, pooled: p, reachable: reach}
			h.entries[n.ID] = e
		}
		e.node = n
		h.order = append(h.order, n.ID)
		out = append(out, e)
	}
	for id := range h.entries {
		if !seen[id] {
			delete(h.entries, id)
		}
	}
	return out, nil
}

func (h *hostSet) setReachable(id uuid.UUID, ok bool, st agentapi.PoolerStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, found := h.entries[id]; found {
		e.reachable = ok
		if ok {
			e.status = st
		}
	}
}

// reachableAdmins are the admin consoles of hosts last seen reachable, in
// region (any region when empty).
func (h *hostSet) reachableAdmins(region string) []*Admin {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*Admin
	for _, id := range h.order {
		if e := h.entries[id]; e != nil && e.reachable && (region == "" || e.node.Region == region) {
			out = append(out, e.session, e.pooled)
		}
	}
	return out
}

// readBundle collects a region's pooler files: the routes and auth file
// Sync writes in its directory, and the TLS pair there (a region with its
// own hostname) or else the one tlscert writes in the main directory.
func readBundle(dir, fallback string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, name := range agentapi.PoolerFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) && dir != fallback && (name == "server.crt" || name == "server.key") {
			b, err = os.ReadFile(filepath.Join(fallback, name))
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		files[name] = b
	}
	return files, nil
}

// HostPushError lists the pooler hosts that did not take a configuration.
// Their copy is stale, so they fail keepalived's check and can't take the
// floating IP; the arbiter pushes again until they catch up.
type HostPushError struct {
	Failed map[string]error // by host name
	// All is true when no host took the configuration.
	All bool
}

func (e *HostPushError) Error() string {
	var errs []error
	for name, err := range e.Failed {
		errs = append(errs, fmt.Errorf("pooler host %s: %w", name, err))
	}
	return errors.Join(errs...).Error()
}

// pushHosts sends the current files to every pooler host, then tells all
// of them the generation. It returns nil when there are no hosts or every
// host took it, a *HostPushError otherwise.
func (m *Manager) pushHosts(ctx context.Context) error {
	hs := m.hostSet()
	if hs == nil {
		return nil
	}
	entries, err := hs.refresh(ctx)
	if err != nil {
		return fmt.Errorf("pooler hosts: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	// Each region's hosts get that region's configuration and generation.
	byRegion := map[string][]*poolerHostEntry{}
	for _, e := range entries {
		byRegion[e.node.Region] = append(byRegion[e.node.Region], e)
	}
	type result struct {
		e   *poolerHostEntry
		st  agentapi.PoolerStatus
		err error
	}
	failed := map[string]error{}
	for region, group := range byRegion {
		files, err := readBundle(m.RegionDir(region), m.dir)
		if err != nil {
			return fmt.Errorf("pooler bundle for %s: %w", region, err)
		}
		hash := agentapi.PoolerHash(files)
		cfg, err := store.New(m.db).AdvancePoolerGeneration(ctx, store.AdvancePoolerGenerationParams{Region: region, Hash: hash})
		if err != nil {
			return fmt.Errorf("pooler generation: %w", err)
		}
		b := agentapi.PoolerBundle{Generation: cfg.Generation, Hash: hash, Files: files}
		results := make(chan result, len(group))
		for _, e := range group {
			go func() {
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				st, err := hs.driver.Push(pctx, e.node, b)
				results <- result{e, st, err}
			}()
		}
		for range group {
			r := <-results
			hs.setReachable(r.e.node.ID, r.err == nil, r.st)
			if r.err != nil {
				failed[r.e.node.Name] = r.err
				m.event(ctx, &r.e.node.ID, "push_failed", map[string]any{"generation": b.Generation, "region": region, "error": r.err.Error()})
			}
		}
		// Every host learns the generation, including any that just failed,
		// so a host that missed this push knows it is stale.
		for _, e := range group {
			ectx, cancel := context.WithTimeout(ctx, 5*time.Second)
			st, err := hs.driver.Expect(ectx, e.node, agentapi.PoolerExpected{Generation: b.Generation, Hash: hash})
			cancel()
			if err == nil {
				hs.setReachable(e.node.ID, true, st)
			}
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return &HostPushError{Failed: failed, All: len(failed) == len(entries)}
}

// event records a pooler event; failures to record are only logged.
// PoolerAddrs are the addresses the poolers connect from, as far as
// xenibase-server knows them: its local poolers' hosts and every pooler
// host's private address, names resolved. The waker admits only these.
func (m *Manager) PoolerAddrs(ctx context.Context) []netip.Addr {
	var hosts []string
	for _, a := range m.admins {
		hosts = append(hosts, a.cfg.Host)
	}
	if hs := m.hostSet(); hs != nil {
		if entries, err := hs.refresh(ctx); err == nil {
			for _, e := range entries {
				hosts = append(hosts, e.node.PrivateAddr)
			}
		} else {
			m.log.Warn("pooler addresses: list the pooler hosts", "err", err)
		}
	}
	var out []netip.Addr
	for _, h := range hosts {
		if a, err := netip.ParseAddr(h); err == nil {
			out = append(out, a.Unmap())
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		as, err := net.DefaultResolver.LookupNetIP(rctx, "ip", h)
		cancel()
		for _, a := range as {
			out = append(out, a.Unmap())
		}
		if err != nil {
			m.log.Debug("pooler addresses: resolve", "host", h, "err", err)
		}
	}
	return out
}

func (m *Manager) event(ctx context.Context, node *uuid.UUID, kind string, detail map[string]any) {
	d, _ := json.Marshal(detail)
	var id *uuid.UUID
	if node != nil {
		id = node
	}
	if err := store.New(m.db).InsertPoolerEvent(context.WithoutCancel(ctx), store.InsertPoolerEventParams{NodeID: id, Kind: kind, Detail: d}); err != nil {
		m.log.Warn("could not record pooler event", "kind", kind, "err", err)
	}
}
