package pooler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/store"
)

// syncLockKey serializes pooler syncs across xenibase-server processes.
const syncLockKey int64 = 0x7067646f636b01 // "xenibase\x01"

// Manager keeps the poolers' generated files in step with the metadata DB.
type Manager struct {
	dir    string
	mode   os.FileMode
	db     *pgxpool.Pool
	admins []*Admin
	static []User // e.g. the admin console user
	log    *slog.Logger

	mu sync.Mutex

	hostsMu sync.Mutex
	hosts   *hostSet // pooler hosts (V3 §2.1), when set

	// The home region (V3 §6.1): the local poolers and the files in dir
	// serve it; other regions' files are in dir/regions/<id>. dbRegions is
	// which regions route each database, from the last Sync.
	homeMu    sync.Mutex
	home      string
	routesMu  sync.Mutex
	dbRegions map[string]map[string]bool
	// ownPoolers: the regions with pooler hosts of their own (and home).
	ownPoolers map[string]bool

	// The waker (V3 §4.2), as the poolers reach it: paused and archived
	// projects route there. Empty: they keep their usual route. Its own
	// lock: m.mu is held through a Sync, which waits for the database.
	wakerMu   sync.Mutex
	wakerHost string
	wakerPort int

	// adminNets may reach the admin consoles (hba.conf); nil means
	// DefaultAdminNetworks.
	adminNets []string
}

// SetAdminNetworks sets the networks the poolers' admin consoles accept
// (XENIBASE_POOLER_ADMIN_ALLOW). Call it before the first Sync.
func (m *Manager) SetAdminNetworks(cidrs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adminNets = append([]string(nil), cidrs...)
}

// SetHome sets the home region (default eu-central).
func (m *Manager) SetHome(region string) {
	m.homeMu.Lock()
	defer m.homeMu.Unlock()
	m.home = region
}

// Home is the home region.
func (m *Manager) Home() string {
	m.homeMu.Lock()
	defer m.homeMu.Unlock()
	if m.home == "" {
		return "eu-central"
	}
	return m.home
}

// RegionDir is where region's pooler files are: the config directory for
// the home region, else a directory under it.
func (m *Manager) RegionDir(region string) string {
	if region == "" || region == m.Home() {
		return m.dir
	}
	return filepath.Join(m.dir, "regions", region)
}

// PoolerRegion is the region whose poolers serve region's projects: its
// own, or the home region's when it has no pooler hosts.
func (m *Manager) PoolerRegion(region string) string {
	m.routesMu.Lock()
	defer m.routesMu.Unlock()
	if region == "" || m.ownPoolers == nil || m.ownPoolers[region] {
		if region == "" {
			return m.Home()
		}
		return region
	}
	return m.Home()
}

// SmokeAddrs are the session and transaction pooler addresses of one of
// region's own pooler hosts, for the provisioning smoke test; ok is false
// when region is served by the home poolers (use the local addresses).
func (m *Manager) SmokeAddrs(region string) (session, pooled string, ok bool) {
	pr := m.PoolerRegion(region)
	if pr == m.Home() {
		return "", "", false
	}
	hs := m.hostSet()
	if hs == nil {
		return "", "", false
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	for _, id := range hs.order {
		if e := hs.entries[id]; e != nil && e.reachable && e.node.Region == pr {
			return net.JoinHostPort(e.node.PrivateAddr, strconv.Itoa(hs.admin.SessionPort)),
				net.JoinHostPort(e.node.PrivateAddr, strconv.Itoa(hs.admin.PooledPort)), true
		}
	}
	return "", "", false
}

// routedIn reports whether region's poolers route db (true when unknown:
// no Sync yet).
func (m *Manager) routedIn(db, region string) bool {
	m.routesMu.Lock()
	defer m.routesMu.Unlock()
	if m.dbRegions == nil {
		return true
	}
	regs, ok := m.dbRegions[db]
	if !ok {
		return region == m.Home()
	}
	return regs[region]
}

// SetWaker routes paused and archived projects to the waker at host:port.
func (m *Manager) SetWaker(host string, port int) {
	m.wakerMu.Lock()
	defer m.wakerMu.Unlock()
	m.wakerHost, m.wakerPort = host, port
}

// WakerSet reports whether paused projects route to a waker.
func (m *Manager) WakerSet() bool {
	_, port := m.waker()
	return port != 0
}

// WakerAddr is the waker as the poolers reach it, or "" when none is set.
func (m *Manager) WakerAddr() string {
	host, port := m.waker()
	if port == 0 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (m *Manager) waker() (string, int) {
	m.wakerMu.Lock()
	defer m.wakerMu.Unlock()
	return m.wakerHost, m.wakerPort
}

// NewManager returns a Manager writing files with mode into dir and
// reloading admins. staticUsers are always present in the auth file (at
// least the admin console user, or xenibase-server could not reach the
// poolers).
func NewManager(dir string, mode os.FileMode, db *pgxpool.Pool, admins []*Admin, staticUsers []User, log *slog.Logger) (*Manager, error) {
	if dir == "" {
		return nil, errors.New("pooler: config directory is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("pooler: %w", err)
	}
	return &Manager{dir: dir, mode: mode, db: db, admins: admins, static: staticUsers, log: log}, nil
}

// Admins returns the pooler admin consoles: the local poolers, and both
// PgBouncers of every pooler host last seen reachable.
func (m *Manager) Admins() []*Admin {
	out := append([]*Admin(nil), m.admins...)
	if hs := m.hostSet(); hs != nil {
		out = append(out, hs.reachableAdmins("")...)
	}
	return out
}

// adminsFor are the admin consoles of the poolers that route db: the local
// ones for the home region's databases, and the pooler hosts of each region
// that routes it (V3 §6.1). Commands to the others would fail: they don't
// know the database.
func (m *Manager) adminsFor(db string) []*Admin {
	var out []*Admin
	if m.routedIn(db, m.Home()) {
		out = append(out, m.admins...)
	}
	hs := m.hostSet()
	if hs == nil {
		return out
	}
	m.routesMu.Lock()
	regs := m.dbRegions[db]
	known := m.dbRegions != nil
	m.routesMu.Unlock()
	if !known {
		return append(out, hs.reachableAdmins("")...)
	}
	if len(regs) == 0 {
		return append(out, hs.reachableAdmins(m.Home())...)
	}
	for r := range regs {
		out = append(out, hs.reachableAdmins(r)...)
	}
	return out
}

// Sync renders routes and the auth file from the metadata DB, writes them
// atomically, and RELOADs every pooler. It always reloads, so a pooler that
// missed an earlier reload catches up.
func (m *Manager) Sync(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	conn, err := m.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("pooler sync: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", syncLockKey); err != nil {
		return fmt.Errorf("pooler sync lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", syncLockKey)
	}()

	q := store.New(conn)
	regions, err := q.ListRegions(ctx)
	if err != nil {
		return fmt.Errorf("pooler sync: load regions: %w", err)
	}
	// One configuration per region: its own projects, and for 30 days
	// those moved away from it (V3 §6.1). Projects with data residency are
	// never routed by another region's poolers once their forward ends.
	home := m.Home()
	cfgs := map[string]*Config{home: {Users: append([]User(nil), m.static...)}}
	for _, r := range regions {
		if cfgs[r.ID] == nil {
			cfgs[r.ID] = &Config{Users: append([]User(nil), m.static...)}
		}
	}
	// A region without pooler hosts of its own (a new region, or a
	// single-box install) is served by the home region's poolers.
	hosts, err := q.PoolerHosts(ctx)
	if err != nil {
		return fmt.Errorf("pooler sync: load pooler hosts: %w", err)
	}
	ownPoolers := map[string]bool{home: true}
	for _, h := range hosts {
		ownPoolers[h.Region] = true
	}
	poolerRegion := func(region string) string {
		if ownPoolers[region] {
			return region
		}
		return home
	}
	m.routesMu.Lock()
	m.ownPoolers = ownPoolers
	m.routesMu.Unlock()
	now := time.Now()
	served := func(region string, fwd *string, until *time.Time) []string {
		out := []string{poolerRegion(region)}
		if fwd != nil && until != nil && until.After(now) && poolerRegion(*fwd) != out[0] {
			out = append(out, poolerRegion(*fwd))
		}
		return out
	}
	add := func(regions []string, f func(*Config)) {
		for _, r := range regions {
			c := cfgs[r]
			if c == nil {
				c = &Config{Users: append([]User(nil), m.static...)}
				cfgs[r] = c
			}
			f(c)
		}
	}
	dbRegions := map[string]map[string]bool{}
	mark := func(db string, regions []string) {
		if dbRegions[db] == nil {
			dbRegions[db] = map[string]bool{}
		}
		for _, r := range regions {
			dbRegions[db][r] = true
		}
	}

	rows, err := q.PoolerRoutes(ctx)
	if err != nil {
		return fmt.Errorf("pooler sync: load routes: %w", err)
	}
	// Read replicas (V4 §7), by project database.
	repRows, err := q.PoolerReplicaRoutes(ctx)
	if err != nil {
		return fmt.Errorf("pooler sync: load read replicas: %w", err)
	}
	replicas := map[string][]store.PoolerReplicaRoutesRow{}
	for _, r := range repRows {
		replicas[r.DbName] = append(replicas[r.DbName], r)
	}
	wakerHost, wakerPort := m.waker()
	for _, r := range rows {
		s, err := store.DecodeProjectSettings(r.Settings)
		if err != nil {
			return fmt.Errorf("pooler sync: %s: %w", r.DbName, err)
		}
		host, port := r.Host, int(r.Port)
		if r.Lifecycle != "active" && wakerHost != "" {
			// Asleep: the waker answers, and wakes the project (V3 §4.2).
			host, port = wakerHost, wakerPort
			s.PoolSize, s.ConnectionLimit = 1, 1
		}
		regs := served(r.Region, r.ForwardRegion, r.ForwardUntil)
		mark(r.DbName, regs)
		if _, ok := replicas[r.DbName]; ok {
			mark(r.DbName+ReadOnlySuffix, regs)
		}
		addIn := func(regions []string, f func(cfg *Config, region string)) {
			for _, rg := range regions {
				add([]string{rg}, func(cfg *Config) { f(cfg, rg) })
			}
		}
		addIn(regs, func(cfg *Config, region string) {
			cfg.Routes = append(cfg.Routes, Route{
				Database:         r.DbName,
				Host:             host,
				Port:             port,
				PoolSize:         s.PoolSize,
				MaxDBConnections: s.ConnectionLimit,
			})
			cfg.Users = append(cfg.Users, User{Name: r.OwnerRole, Secret: r.ScramVerifier})
			// A V1 project renamed to an opaque database keeps its old name
			// as an alias, so existing connection strings still work (V2
			// §10.2).
			if r.AliasDbName != nil {
				cfg.Routes = append(cfg.Routes, Route{
					Database: *r.AliasDbName, BackendDB: r.DbName, Host: host, Port: port,
					PoolSize: s.PoolSize, MaxDBConnections: s.ConnectionLimit,
				})
			}
			// A project with read replicas gets <db>_ro across those in
			// rotation, or its primary when none is (V4 §7).
			if reps, ok := replicas[r.DbName]; ok {
				ro := Route{Database: r.DbName + ReadOnlySuffix, BackendDB: r.DbName, Host: host, Port: port,
					PoolSize: s.PoolSize, MaxDBConnections: s.ConnectionLimit, ReadOnly: true}
				if r.Lifecycle == "active" {
					if hosts, p := replicaHosts(reps, region, poolerRegion); len(hosts) > 0 {
						ro.Host, ro.MoreHosts, ro.Port = hosts[0], hosts[1:], p
					}
				}
				cfg.Routes = append(cfg.Routes, ro)
			}
			// The V1 owner role during a switch to opaque credentials.
			if r.LegacyOwnerRole != nil && r.LegacyScramVerifier != nil {
				cfg.Users = append(cfg.Users, User{Name: *r.LegacyOwnerRole, Secret: *r.LegacyScramVerifier})
			}
		})
		if r.AliasDbName != nil {
			mark(*r.AliasDbName, regs)
		}
	}
	// IP allowlists: the project's routes take its customer logins only
	// from the listed networks; its edge, request-role and SLA probe
	// logins are Xenibase's own and connect from wherever Xenibase runs.
	for _, r := range rows {
		if len(r.AllowedCidrs) == 0 {
			continue
		}
		res := Restriction{Databases: []string{r.DbName, r.DbName + ReadOnlySuffix}, CIDRs: r.AllowedCidrs,
			Platform: append(store.ServiceRoles(r.DbName), store.ProbeRole(r.DbName))}
		if r.AliasDbName != nil {
			res.Databases = append(res.Databases, *r.AliasDbName)
		}
		add(served(r.Region, r.ForwardRegion, r.ForwardUntil), func(cfg *Config) {
			cfg.Restrictions = append(cfg.Restrictions, res)
		})
	}
	members, err := q.PoolerDBUsers(ctx)
	if err != nil {
		return fmt.Errorf("pooler sync: load personal logins: %w", err)
	}
	for _, u := range members {
		add(served(u.Region, u.ForwardRegion, u.ForwardUntil), func(cfg *Config) {
			cfg.Users = append(cfg.Users, User{Name: u.RoleName, Secret: u.ScramVerifier})
		})
	}
	// HA projects' SLA probe logins (V3 §2.7).
	probes, err := q.PoolerProbeUsers(ctx)
	if err != nil {
		return fmt.Errorf("pooler sync: load probe logins: %w", err)
	}
	for _, u := range probes {
		add(served(u.Region, u.ForwardRegion, u.ForwardUntil), func(cfg *Config) {
			cfg.Users = append(cfg.Users, User{Name: store.ProbeRole(u.DbName), Secret: u.ProbeVerifier})
		})
	}

	// Backend services' edge logins (V4 §2.3).
	edges, err := q.PoolerEdgeUsers(ctx)
	if err != nil {
		return fmt.Errorf("pooler sync: load edge logins: %w", err)
	}
	for _, u := range edges {
		logins := map[string]string{}
		if len(u.LoginVerifiers) > 0 {
			if err := json.Unmarshal(u.LoginVerifiers, &logins); err != nil {
				return fmt.Errorf("pooler sync: %s's login verifiers: %w", u.DbName, err)
			}
		}
		add(served(u.Region, u.ForwardRegion, u.ForwardUntil), func(cfg *Config) {
			cfg.Users = append(cfg.Users, User{Name: store.EdgeRole(u.DbName), Secret: u.EdgeVerifier})
			// The request roles' logins (V4-M32).
			for _, role := range store.RequestRoles(u.DbName) {
				if v := logins[role]; v != "" {
					cfg.Users = append(cfg.Users, User{Name: role, Secret: v})
				}
			}
		})
	}

	// Logins dedicated owners created with role management on, last: a
	// platform login with the same name wins (Render refuses duplicates).
	owned, err := q.PoolerOwnedLogins(ctx)
	if err != nil {
		return fmt.Errorf("pooler sync: load owned logins: %w", err)
	}
	for _, u := range owned {
		add(served(u.Region, u.ForwardRegion, u.ForwardUntil), func(cfg *Config) {
			if !slices.ContainsFunc(cfg.Users, func(x User) bool { return x.Name == u.RoleName }) {
				cfg.Users = append(cfg.Users, User{Name: u.RoleName, Secret: u.ScramVerifier})
			}
		})
	}

	for region, cfg := range cfgs {
		dir := m.RegionDir(region)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("pooler sync: %w", err)
		}
		cfg.AdminNetworks = m.adminNets
		databases, userlist, hba, err := Render(*cfg)
		if err != nil {
			return err
		}
		// Users first: a new route is only useful once its user can log in.
		usersChanged, err := WriteFileAtomic(filepath.Join(dir, UserlistFile), userlist, m.mode)
		if err != nil {
			return fmt.Errorf("pooler sync: %w", err)
		}
		// Then who may log in from where, so a new route never opens
		// wider than its allowlist.
		if _, err := WriteFileAtomic(filepath.Join(dir, HBAFile), hba, m.mode); err != nil {
			return fmt.Errorf("pooler sync: %w", err)
		}
		routesChanged, err := WriteFileAtomic(filepath.Join(dir, DatabasesFile), databases, m.mode)
		if err != nil {
			return fmt.Errorf("pooler sync: %w", err)
		}
		m.log.Debug("pooler config rendered", "region", region, "routes", len(cfg.Routes), "users_changed", usersChanged, "routes_changed", routesChanged)
	}
	m.routesMu.Lock()
	m.dbRegions = dbRegions
	m.routesMu.Unlock()
	return m.distribute(ctx)
}

// distribute pushes the files to the pooler hosts and RELOADs every
// PgBouncer. A pooler host that misses the push is only logged: it is now
// stale, fails keepalived's check, and the arbiter catches it up. Only when
// no host took the configuration does the change fail.
func (m *Manager) distribute(ctx context.Context) error {
	if err := m.pushHosts(ctx); err != nil {
		var pe *HostPushError
		if errors.As(err, &pe) && !pe.All {
			m.log.Warn("pooler host missed a configuration push; it stays stale until it catches up", "err", err)
		} else {
			return err
		}
	}
	if err := m.each(ctx, func(a *Admin) error { return a.Reload(ctx) }); err != nil {
		return &ReloadError{Err: err}
	}
	return nil
}

// ReloadError means the configuration files were written but at least one
// pooler did not reload them (it reads them when it next starts).
type ReloadError struct{ Err error }

func (e *ReloadError) Error() string { return e.Err.Error() }
func (e *ReloadError) Unwrap() error { return e.Err }

// Dir is the pooler config directory.
func (m *Manager) Dir() string { return m.dir }

// FileMode is the mode generated files are written with.
func (m *Manager) FileMode() os.FileMode { return m.mode }

// Reload makes every pooler re-read its configuration, auth file, and TLS
// certificate without re-rendering anything (pooler hosts get the files
// pushed first, e.g. a renewed certificate).
func (m *Manager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.distribute(ctx)
}

// Kill drops all connections to dbs on every pooler. dbs are a project's
// pooler names: its database and any V1 alias (store.PoolerNames).
func (m *Manager) Kill(ctx context.Context, dbs ...string) error {
	return m.eachDB(ctx, dbs, func(a *Admin, db string) error { return a.Kill(ctx, db) })
}

// Reconnect recycles dbs' server connections on every pooler.
func (m *Manager) Reconnect(ctx context.Context, dbs ...string) error {
	return m.eachDB(ctx, dbs, func(a *Admin, db string) error { return a.Reconnect(ctx, db) })
}

// Pause pauses dbs on every pooler.
func (m *Manager) Pause(ctx context.Context, dbs ...string) error {
	return m.eachDB(ctx, dbs, func(a *Admin, db string) error { return a.Pause(ctx, db) })
}

func (m *Manager) eachDB(ctx context.Context, dbs []string, f func(*Admin, string) error) error {
	var errs []error
	for _, db := range dbs {
		for _, a := range m.adminsFor(db) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := f(a, db); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// SessionPooler names the session-mode pooler's admin.
const SessionPooler = "session"

// Freeze holds db's clients on every pooler before a cutover (spec §6.6
// step 3). On the transaction pooler PAUSE lets in-flight transactions
// finish and queues new work, falling back to KILL if that takes longer
// than wait. Session-mode clients hold their server connection until they
// disconnect, so PAUSE would wait for them: that pooler gets KILL, which
// drops them; their reconnects wait. Resume releases both. It returns the
// poolers that used KILL.
func (m *Manager) Freeze(ctx context.Context, wait time.Duration, dbs ...string) ([]string, error) {
	var killed []string
	var errs []error
	for _, db := range dbs {
		k, err := m.freeze(ctx, db, wait)
		killed = append(killed, k...)
		if err != nil {
			errs = append(errs, err)
		}
		if ctx.Err() != nil {
			return killed, ctx.Err()
		}
	}
	return killed, errors.Join(errs...)
}

func (m *Manager) freeze(ctx context.Context, db string, wait time.Duration) ([]string, error) {
	var killed []string
	var errs []error
	for _, a := range m.adminsFor(db) {
		if a.Name == SessionPooler || strings.HasPrefix(a.Name, SessionPooler+"@") {
			if err := a.Kill(ctx, db); err != nil {
				errs = append(errs, err)
				continue
			}
			killed = append(killed, a.Name)
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, wait)
		err := a.Pause(pctx, db)
		cancel()
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return killed, ctx.Err()
		}
		if kerr := a.Kill(ctx, db); kerr != nil {
			errs = append(errs, fmt.Errorf("pause: %w; kill: %w", err, kerr))
			continue
		}
		killed = append(killed, a.Name)
	}
	return killed, errors.Join(errs...)
}

// Resume resumes dbs on every pooler.
func (m *Manager) Resume(ctx context.Context, dbs ...string) error {
	return m.eachDB(ctx, dbs, func(a *Admin, db string) error { return a.Resume(ctx, db) })
}

func (m *Manager) each(ctx context.Context, f func(*Admin) error) error {
	var errs []error
	for _, a := range m.Admins() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f(a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DropDisallowed drops p's client connections that its IP allowlist,
// cidrs, no longer admits: on its routes (the database, its _ro route and
// V1 alias), from an address outside every network, apart from Xenibase's
// own logins, which the allowlist exempts. PgBouncer checks HBA rules only
// at login, so narrowing a list does nothing to clients already connected
// until this runs. An empty list admits everyone and drops nothing. It
// returns how many clients it dropped.
func (m *Manager) DropDisallowed(ctx context.Context, p store.Project, cidrs []string) (int, error) {
	if len(cidrs) == 0 {
		return 0, nil
	}
	nets := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		pfx, err := netip.ParsePrefix(c)
		if err != nil {
			return 0, fmt.Errorf("pooler: allowlist entry %q: %w", c, err)
		}
		nets = append(nets, pfx)
	}
	dbs := map[string]bool{}
	for _, d := range store.PoolerNames(p) {
		dbs[d], dbs[d+ReadOnlySuffix] = true, true
	}
	exempt := map[string]bool{store.ProbeRole(p.DbName): true}
	for _, r := range store.ServiceRoles(p.DbName) {
		exempt[r] = true
	}
	drop := func(c Client) bool { return dbs[c.Database] && !exempt[c.User] && !admitted(c.Addr, nets) }
	seen := map[*Admin]bool{}
	var errs []error
	n := 0
	for _, d := range store.PoolerNames(p) {
		for _, a := range m.adminsFor(d) {
			if seen[a] {
				continue
			}
			seen[a] = true
			k, err := a.KillClients(ctx, drop)
			n += k
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	return n, errors.Join(errs...)
}

// admitted reports whether a client at addr is on one of nets. A socket
// client ("unix") is local, never subject to an allowlist.
func admitted(addr string, nets []netip.Prefix) bool {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return addr == "unix"
	}
	a = a.Unmap()
	for _, n := range nets {
		if n.Contains(a) {
			return true
		}
	}
	return false
}

// KillUser drops user's client connections on every pooler.
func (m *Manager) KillUser(ctx context.Context, user string) error {
	return m.each(ctx, func(a *Admin) error { _, err := a.KillUser(ctx, user); return err })
}

// replicaHosts picks the read replicas a region's read-only route balances
// across: those in rotation whose pooler region is this one, or, when none
// is, every replica in rotation (still reads off the primary). PgBouncer
// takes one port for a host list, so it is the largest group sharing a
// port (replicas ask for the same one; see dedicated.replicaPort).
func replicaHosts(reps []store.PoolerReplicaRoutesRow, region string, poolerRegion func(string) string) ([]string, int) {
	pick := func(local bool) ([]string, int) {
		byPort := map[int][]string{}
		best := 0
		for _, r := range reps {
			if !r.InRotation || r.Port <= 0 || (local && poolerRegion(r.Region) != region) {
				continue
			}
			p := int(r.Port)
			byPort[p] = append(byPort[p], r.Host)
			if best == 0 || len(byPort[p]) > len(byPort[best]) {
				best = p
			}
		}
		return byPort[best], best
	}
	if hosts, port := pick(true); len(hosts) > 0 {
		return hosts, port
	}
	return pick(false)
}
