// Package edge is xenibase-edge (V4 §2.1): the per-region gateway that
// serves every project's backend services at https://<ref>.<domain>. It
// keeps an in-memory copy of each project's configuration, followed from
// xenibase-server's feed, resolves exactly one project per request from the
// hostname, checks the API key (and a user's token) against that project
// only, and runs the request in one transaction through the edge pooler,
// as the project's anon, user or service role with the caller's claims
// set locally. It meters requests and sends request logs back.
package edge

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/jwtes"
)

// Config is xenibase-edge's configuration.
type Config struct {
	// Name identifies this edge in reports (its hostname).
	Name string
	// ControlURL and Secret reach xenibase-server's edge feed.
	ControlURL string
	Secret     string
	// Region limits the edge to one region's projects ("" for all).
	Region string
	// Domain is the hostname suffix: <ref>.<Domain>.
	Domain string
	// PoolerAddr overrides the pooler address the feed gives (host:port),
	// for an edge that reaches the pooler another way (the mesh).
	PoolerAddr string
	// SessionAddr overrides the session-mode pooler address the feed gives,
	// for realtime's LISTEN connections.
	SessionAddr string
	// PoolerSSLMode is the sslmode to the pooler (default "require").
	// verify-full checks the pooler's certificate and hostname: against the
	// system's roots, or PoolerRootCert.
	PoolerSSLMode string
	// PoolerRootCert is a PEM file of the CA that signed the poolers'
	// certificate (sslrootcert), for verify-ca or verify-full with a
	// private or self-signed certificate.
	PoolerRootCert string
	// TrustedProxies may set X-Forwarded-For.
	TrustedProxies []netip.Prefix
	// PollWait, ResyncEvery and ReportEvery time the feed and reports.
	PollWait    time.Duration
	ResyncEvery time.Duration
	ReportEvery time.Duration
	// AliveEvery is the longest the edge goes without a report, sending an
	// empty one when it has nothing to say (default 30s), so xenibase-server
	// sees it is up (V4.1 §7.2).
	AliveEvery time.Duration
	// AuthRateScale multiplies the per-IP limits on auth endpoints (tests
	// sign in many times from one address); 0 means 1.
	AuthRateScale int
	// HTTPClient reaches OAuth providers and the captcha service (default:
	// a 10-second client).
	HTTPClient *http.Client
	// OAuthEndpoints overrides providers' endpoints (tests' fake providers).
	OAuthEndpoints map[string]OAuthEndpoints
	// GarbageGrace is how long replaced and deleted objects' bytes stay, for
	// downloads in flight (default a minute).
	GarbageGrace time.Duration
	// CacheBytes bounds the anonymous-read cache (default 64 MB).
	CacheBytes int
	// RenderWorker is the command a render worker runs as (V4.1 §12.4),
	// e.g. {"/usr/local/bin/xenibase-edge", "render-worker"}; empty renders
	// in the edge's own process.
	RenderWorker []string
	// RenderWorkerEnv is added to the workers' environment (GOMEMLIMIT,
	// the address-space cap, a test's crash marker).
	RenderWorkerEnv []string
	Log             *slog.Logger
}

func (c *Config) defaults() {
	if c.PollWait == 0 {
		c.PollWait = 25 * time.Second
	}
	if c.ResyncEvery == 0 {
		c.ResyncEvery = time.Minute
	}
	if c.ReportEvery == 0 {
		c.ReportEvery = 10 * time.Second
	}
	if c.AliveEvery == 0 {
		c.AliveEvery = 30 * time.Second
	}
	if c.PoolerSSLMode == "" {
		c.PoolerSSLMode = "require"
	}
	if c.GarbageGrace == 0 {
		c.GarbageGrace = time.Minute
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	c.Domain = strings.ToLower(strings.Trim(c.Domain, "."))
}

// Edge is a running gateway.
type Edge struct {
	cfg    Config
	client *edgeapi.Client

	mu     sync.RWMutex
	byRef  map[string]*project
	seq    int64
	ready  bool
	meter  *meter
	limits *limiter
	cache  *respCache
	cpu    cpuSampler
	// renders are the render workers; nil renders in process.
	renders *renderPool
	mau     mauSeen
	waking  sync.Map // ref -> time.Time of the last wake asked
	// hashSlots bound concurrent password hashes, renderSlots image
	// transforms.
	hashSlots   chan struct{}
	renderSlots chan struct{}
	// Realtime's per-project state (rthub.go).
	rtMu     sync.Mutex
	hubs     map[string]*rtHub
	nodeOnce sync.Once
	nodeID   string
	// life ends with Run: realtime's listeners stop with it.
	life context.Context
}

// project is one project's configuration and its database pool.
type project struct {
	cfg  edgeapi.Project
	keys map[string]edgeapi.Key // by hash
	jwks map[string]*ecdsa.PublicKey
	// db is shared with the next copy of the configuration while how it
	// connects doesn't change; catalog always is.
	db      *dbconn
	catalog *catalogState
	// files is the object store client, kept while the store is the same.
	files *fileStore
}

// exposed are the schemas the data API serves.
func (p *project) exposed() []string {
	if len(p.cfg.ExposedSchemas) == 0 {
		return []string{"public"}
	}
	return p.cfg.ExposedSchemas
}

// publicTable reports whether t may be read without row-level security.
func (p *project) publicTable(t *Table) bool {
	for _, n := range p.cfg.PublicTables {
		if n == t.Schema+"."+t.Name || (t.Schema == "public" && n == t.Name) {
			return true
		}
	}
	return false
}

type dbconn struct {
	mu sync.Mutex
	// pools by login: "" is the edge login (auth), the others the request
	// roles' and the hook role's own logins.
	pools  map[string]*pgxpool.Pool
	closed bool
}

func (d *dbconn) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	for k, p := range d.pools {
		p.Close()
		delete(d.pools, k)
	}
}

// New returns an edge; Run starts following the feed and reporting.
func New(cfg Config) *Edge {
	cfg.defaults()
	e := &Edge{
		cfg:         cfg,
		client:      &edgeapi.Client{URL: cfg.ControlURL, Secret: cfg.Secret},
		byRef:       map[string]*project{},
		meter:       newMeter(),
		limits:      newLimiter(),
		cache:       newRespCache(cfg.CacheBytes),
		hashSlots:   make(chan struct{}, max(2, runtime.GOMAXPROCS(0))),
		renderSlots: make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/2)),
	}
	if len(cfg.RenderWorker) > 0 {
		e.renders = newRenderPool(cfg.RenderWorker, cfg.RenderWorkerEnv, cap(e.renderSlots), cfg.Log)
	}
	return e
}

// RenderWorkersStarted is how many render workers the edge has started
// (tests watch a crashed one replaced).
func (e *Edge) RenderWorkersStarted() int {
	if e.renders == nil {
		return 0
	}
	return e.renders.Started()
}

func (e *Edge) httpClient() *http.Client {
	if e.cfg.HTTPClient != nil {
		return e.cfg.HTTPClient
	}
	return defaultHTTP
}

var defaultHTTP = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

// Run follows the configuration feed and sends reports until ctx ends.
func (e *Edge) Run(ctx context.Context) {
	e.rtMu.Lock()
	e.life = ctx
	e.rtMu.Unlock()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); e.follow(ctx) }()
	go func() { defer wg.Done(); e.report(ctx) }()
	go func() {
		defer wg.Done()
		t := time.NewTicker(rtMeterEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				e.meterRealtime()
				return
			case <-t.C:
				e.meterRealtime()
			}
		}
	}()
	wg.Wait()
	e.closePools()
	if e.renders != nil {
		e.renders.close()
	}
	e.flush(context.Background())
}

// Ready reports whether the first full configuration has loaded.
func (e *Edge) Ready() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.ready
}

func (e *Edge) follow(ctx context.Context) {
	backoff := time.Second
	lastFull := time.Time{}
	for ctx.Err() == nil {
		var err error
		if time.Since(lastFull) >= e.cfg.ResyncEvery {
			if err = e.resync(ctx); err == nil {
				lastFull = time.Now()
			}
		} else {
			err = e.poll(ctx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.cfg.Log.Warn("edge configuration feed", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, 30*time.Second)
			continue
		}
		backoff = time.Second
	}
}

// resync reads the whole feed from the start: the backstop for any change
// an incremental poll could have missed.
func (e *Edge) resync(ctx context.Context) error {
	var since int64
	for {
		c, err := e.client.Config(ctx, e.cfg.Region, since, 0)
		if err != nil {
			return err
		}
		e.apply(c.Projects)
		since = c.Next
		if !c.More {
			break
		}
	}
	e.mu.Lock()
	if since > e.seq {
		e.seq = since
	}
	e.ready = true
	e.mu.Unlock()
	return nil
}

func (e *Edge) poll(ctx context.Context) error {
	e.mu.RLock()
	since := e.seq
	e.mu.RUnlock()
	c, err := e.client.Config(ctx, e.cfg.Region, since, e.cfg.PollWait)
	if err != nil {
		return err
	}
	e.apply(c.Projects)
	e.mu.Lock()
	if c.Next > e.seq {
		e.seq = c.Next
	}
	e.mu.Unlock()
	return nil
}

// apply updates the cache. A project's pool is kept unless how it connects
// changed.
func (e *Edge) apply(ps []edgeapi.Project) {
	refs := make([]string, 0, len(ps))
	defer func() {
		// Realtime's open connections follow the change (rthub.go).
		go e.rtProjectsChanged(refs)
	}()
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, pc := range ps {
		old := e.byRef[pc.Ref]
		if old != nil && old.cfg.Version > pc.Version {
			continue // an older copy (from a resync racing a poll)
		}
		refs = append(refs, pc.Ref)
		if pc.State == edgeapi.StateDisabled {
			if old != nil {
				delete(e.byRef, pc.Ref)
				go old.close()
			}
			continue
		}
		np := &project{cfg: pc, keys: map[string]edgeapi.Key{}, jwks: map[string]*ecdsa.PublicKey{}, db: &dbconn{},
			catalog: &catalogState{}}
		for _, k := range pc.Keys {
			np.keys[k.Hash] = k
		}
		for _, raw := range pc.JWKs {
			var j jwtes.JWK
			if json.Unmarshal(raw, &j) != nil {
				continue
			}
			if pub, err := j.Public(); err == nil {
				np.jwks[j.Kid] = pub
			}
		}
		if pc.Storage != nil {
			np.files = &fileStore{}
		}
		if old != nil {
			np.catalog = old.catalog
			if old.files != nil && sameStore(old.cfg.Storage, pc.Storage) {
				np.files = old.files
			}
			if sameConn(old.cfg, pc) {
				np.db = old.db
			} else {
				go old.close()
			}
		}
		e.byRef[pc.Ref] = np
	}
}

// poolerURL is the connection string to a pooler, with the edge's TLS
// settings.
func (e *Edge) poolerURL(user, password, host string, port int, database, app string) string {
	q := url.Values{"sslmode": {e.cfg.PoolerSSLMode}, "application_name": {app}}
	if e.cfg.PoolerRootCert != "" {
		q.Set("sslrootcert", e.cfg.PoolerRootCert)
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password),
		Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/" + database, RawQuery: q.Encode()}
	return u.String()
}

func sameConn(a, b edgeapi.Project) bool {
	if a.Database != b.Database || a.ReadDatabase != b.ReadDatabase || a.EdgeUser != b.EdgeUser || a.Password != b.Password ||
		a.PoolerHost != b.PoolerHost || a.PoolerPort != b.PoolerPort || len(a.Logins) != len(b.Logins) {
		return false
	}
	for k, v := range a.Logins {
		if b.Logins[k] != v {
			return false
		}
	}
	return true
}

func (p *project) close() { p.db.close() }

func (e *Edge) closePools() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.byRef {
		p.close()
	}
}

// lookup is the project ref names, or nil.
func (e *Edge) lookup(ref string) *project {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.byRef[ref]
}

// dbPool is the project's pool, connecting as its edge login through the
// pooler in transaction mode.
func (e *Edge) dbPool(ctx context.Context, p *project) (*pgxpool.Pool, error) {
	return e.loginPool(ctx, p, "", false)
}

// loginPool is the project's pool for login: "" is the edge login; a
// request role connects as itself when the feed gave its password.
func (e *Edge) loginPool(ctx context.Context, p *project, login string, replica bool) (*pgxpool.Pool, error) {
	d := p.db
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errConfigChanged
	}
	database, key := p.cfg.Database, login
	if replica && p.cfg.ReadDatabase != "" {
		// The read-only route across the read replicas (V4 §7).
		database, key = p.cfg.ReadDatabase, "ro:"+login
	}
	if pool := d.pools[key]; pool != nil {
		return pool, nil
	}
	user, password, maxConns := p.cfg.EdgeUser, p.cfg.Password, int32(4)
	if login != "" {
		pw, ok := p.cfg.Logins[login]
		if !ok {
			return nil, errNoLogin
		}
		user, password = login, pw
		if login == p.cfg.ServiceRole || login == p.cfg.HookRole {
			maxConns = 2
		}
	}
	host, port := p.cfg.PoolerHost, p.cfg.PoolerPort
	if e.cfg.PoolerAddr != "" {
		h, ps, err := net.SplitHostPort(e.cfg.PoolerAddr)
		if err != nil {
			return nil, err
		}
		host = h
		port, _ = strconv.Atoi(ps)
	}
	if host == "" || port == 0 {
		return nil, errors.New("no pooler address for the project's region")
	}
	cfg, err := pgxpool.ParseConfig(e.poolerURL(user, password, host, port, database, "xenibase-edge"))
	if err != nil {
		return nil, err
	}
	// Transaction pooling: no server-side prepared statements kept across
	// transactions.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	cfg.MaxConns, cfg.MinConns = maxConns, 0
	cfg.MaxConnIdleTime, cfg.MaxConnLifetime = time.Minute, 30*time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if d.pools == nil {
		d.pools = map[string]*pgxpool.Pool{}
	}
	d.pools[key] = pool
	return pool, nil
}

// errNoLogin is a feed from a xenibase-server older than M32, without the
// request roles' logins: the edge login switches to the role instead.
var errNoLogin = errors.New("no login for the role")

// rolePool is the pool a role's SQL runs in, and whether the transaction
// must SET ROLE to it (only with an older server's feed).
func (e *Edge) rolePool(ctx context.Context, p *project, role string, replica bool) (*pgxpool.Pool, bool, error) {
	pool, err := e.loginPool(ctx, p, role, replica)
	if errors.Is(err, errNoLogin) {
		pool, err = e.loginPool(ctx, p, "", replica)
		return pool, true, err
	}
	return pool, false, err
}

// errConfigChanged: the project's connection settings changed while a
// request was starting; it is retried by the client.
var errConfigChanged = errors.New("the project's configuration changed; retry")

// Request is who is asking: the role, the claims set for the request, and
// the statement timeout.
type Request struct {
	Role    string         // anon | user | service
	Claims  map[string]any // includes "role"
	Timeout time.Duration
	// Replica runs it on the project's read replicas (V4 §7): reads only.
	Replica bool
}

// dbRole is the project role Role maps to.
func (p *project) dbRole(role string) (string, error) {
	switch role {
	case "anon":
		return p.cfg.AnonRole, nil
	case "user":
		return p.cfg.UserRole, nil
	case "service":
		return p.cfg.ServiceRole, nil
	}
	return "", fmt.Errorf("unknown role %q", role)
}

// WithRequest runs fn in one transaction as req's role with its claims set
// locally (V4 §1.4 "One request, one transaction"), so it works through the
// pooler in transaction mode: nothing outlives the transaction.
func (e *Edge) WithRequest(ctx context.Context, p *project, req Request, fn func(pgx.Tx) error) error {
	role, err := p.dbRole(req.Role)
	if err != nil {
		return err
	}
	// The role's own login: tenant SQL that resets the role stays this role.
	pool, setRole, err := e.rolePool(ctx, p, role, req.Replica)
	if err != nil {
		return err
	}
	claims, err := json.Marshal(req.Claims)
	if err != nil {
		return err
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if setRole {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('xb.claims', $1, true), set_config('statement_timeout', $2, true)`,
			string(claims), strconv.FormatInt(timeout.Milliseconds(), 10)); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Explore serves one data API request for the project pc as role with
// claims, without a key or the rate limits: xenibase-server's request
// explorer. The project's pool is kept between calls.
func (e *Edge) Explore(ctx context.Context, pc edgeapi.Project, role string, claims map[string]any, method, path string, body []byte) (int, http.Header, []byte) {
	e.apply([]edgeapi.Project{pc})
	p := e.lookup(pc.Ref)
	rec := httptest.NewRecorder()
	if p == nil {
		rec.WriteHeader(http.StatusNotFound)
		return rec.Code, rec.Header(), nil
	}
	r, err := http.NewRequestWithContext(ctx, method, "http://"+pc.Ref+".explorer"+path, bytes.NewReader(body))
	if err != nil {
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(http.StatusBadRequest)
		_, _ = rec.WriteString(`{"error":{"code":"invalid_path","message":"the path is malformed"}}`)
		return rec.Code, rec.Header(), rec.Body.Bytes()
	}
	if len(body) > 0 {
		r.Header.Set("Content-Type", "application/json")
	}
	c := &call{id: newRequestID(), start: time.Now(), w: &recorder{ResponseWriter: rec}, r: r, p: p, role: role}
	e.route(c, Request{Role: role, Claims: claims, Timeout: time.Duration(p.cfg.Settings.StatementTimeoutMs) * time.Millisecond})
	return rec.Code, rec.Header(), rec.Body.Bytes()
}
