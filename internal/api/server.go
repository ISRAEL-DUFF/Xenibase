// Package api implements the Xenibase HTTP API and serves the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/alerts"
	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auditlog"
	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/backup"
	"github.com/israel-duff/xenibase/internal/billing"
	"github.com/israel-duff/xenibase/internal/branching"
	"github.com/israel-duff/xenibase/internal/capacity"
	"github.com/israel-duff/xenibase/internal/console"
	"github.com/israel-duff/xenibase/internal/costs"
	"github.com/israel-duff/xenibase/internal/freetier"
	"github.com/israel-duff/xenibase/internal/incidents"
	"github.com/israel-duff/xenibase/internal/insights"
	"github.com/israel-duff/xenibase/internal/isocheck"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/legal"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/pgversions"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/regions"
	"github.com/israel-duff/xenibase/internal/schedjobs"
	"github.com/israel-duff/xenibase/internal/services"
	"github.com/israel-duff/xenibase/internal/settings"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/support"
	"github.com/israel-duff/xenibase/internal/tenancy"
	"github.com/israel-duff/xenibase/internal/tokens"
	"github.com/israel-duff/xenibase/internal/version"
	"github.com/israel-duff/xenibase/internal/webhooks"
)

// Server implements gen.ServerInterface. Endpoints that later milestones
// implement fall through to gen.Unimplemented (501).
type Server struct {
	gen.Unimplemented
	// exporting counts backup downloads running per organisation.
	exportMu  sync.Mutex
	exporting map[uuid.UUID]int
	log       *slog.Logger
	db        DB
	audit     *auditlog.Writer
	streamer  *jobs.Streamer
	projects  *provision.Service
	dev       bool
	auth      *auth.Service
	sec       SecurityOptions
	settings  *settings.Store
	publicIPs []netip.Addr
	tls       func() gen.TlsStatus
	backups   *backup.Service
	nodes     *nodes.Service
	console   *console.Service
	isochecks *isocheck.Service
	alerts    *alerts.Service
	orgs      *orgs.Service
	mail      *mail.Service
	tenancy   *tenancy.Service
	tokens    *tokens.Service
	// services is backend services (V4 §2); nil when not set up.
	services   *services.Service
	branches   *branching.Service
	freetier   *freetier.Service
	support    *support.Service
	legal      *legal.Service
	capacity   *capacity.Service
	costs      *costs.Service
	regions    *regions.Service
	pgversions *pgversions.Service
	insights   *insights.Service
	webhooks   *webhooks.Service
	jobs       *schedjobs.Service
	outbound   *outbound.Service
	incidents  *incidents.Service
	arbiter    *pooler.Arbiter
	billing    *billing.Service

	tokenLimit    *auth.Limiter
	orgTokenLimit *auth.Limiter
	publicBase    string
	clock         func() time.Time

	metricsInterval time.Duration
	metricsToken    string
}

// DB is the metadata database: queries plus a health check.
type DB interface {
	store.DBTX
	Ping(ctx context.Context) error
}

var _ gen.ServerInterface = (*Server)(nil)

// Options configures NewHandler.
type Options struct {
	Logger *slog.Logger
	// DB and Notifier back the operations endpoints and readiness. Both
	// may be nil in tests that only exercise static routes.
	DB       DB
	Notifier *jobs.Notifier
	// DevEndpoints enables /api/v1/dev/* (XENIBASE_DEV_ENDPOINTS).
	DevEndpoints bool
	// StreamCtx ends open SSE streams when done; nil means never.
	StreamCtx context.Context
	// Audit writes audit rows; the caller runs it (Run, then Close at
	// shutdown). Nil means one of NewHandler's own, retried until
	// StreamCtx ends.
	Audit *auditlog.Writer
	// Projects runs provisioning; nil disables the projects endpoints.
	Projects *provision.Service
	// UI is the web UI build output; UIIndex names its entry document.
	UI      fs.FS
	UIIndex string

	// Auth signs operators in and guards every non-public API route.
	// It is required unless InsecureNoAuth is set (unit tests only).
	Auth           *auth.Service
	InsecureNoAuth bool
	Security       SecurityOptions
	// Settings holds the editable DB hostname; PublicIPs are this server's
	// public addresses for the DNS check; TLS reports pooler TLS status.
	Settings  *settings.Store
	PublicIPs []netip.Addr
	TLS       func() gen.TlsStatus
	// Backups and Nodes run backups, restores, imports, and agents; nil
	// disables those endpoints.
	Backups *backup.Service
	Nodes   *nodes.Service
	// Console runs the SQL console, table browser, and extensions; nil
	// disables them. MetricsInterval is the sampling interval (for
	// /metrics freshness); MetricsToken lets scrapers read /metrics.
	Console   *console.Service
	IsoChecks *isocheck.Service
	Alerts    *alerts.Service
	// Orgs runs organisations and memberships; Mail is the platform SMTP.
	Orgs *orgs.Service
	Mail *mail.Service
	// Tenancy runs quotas, storage locks, suspension, break-glass, and usage.
	Tenancy *tenancy.Service
	// Branches runs database branching (V2 §8); nil disables it.
	Branches *branching.Service
	// FreeTier pauses and archives idle Free projects (V3 §4).
	FreeTier *freetier.Service
	// Support runs tickets (V3 §7.1).
	Support *support.Service
	// Legal keeps the SLA, DPA and order forms organisations accept (V3 §7.3).
	Legal *legal.Service
	// Capacity and Costs run capacity automation and cost attribution (V3 §5).
	Capacity *capacity.Service
	Costs    *costs.Service
	// Regions are the platform's regions (V3 §6).
	Regions *regions.Service
	// PGVersions runs the Postgres version lifecycle (V4.1 §6.1).
	PGVersions *pgversions.Service
	// Insights is query insights (V3 §8).
	Insights *insights.Service
	// Webhooks, Jobs and Outbound run database webhooks, scheduled jobs and
	// their outbound requests (V2 §9); nil disables them.
	Webhooks *webhooks.Service
	Jobs     *schedjobs.Service
	Outbound *outbound.Service
	// Incidents backs Admin → Incidents and the status page pushes
	// (V3 §2.6); PoolerArbiter reports the edge pooler hosts (V3 §2.1).
	Incidents     *incidents.Service
	PoolerArbiter *pooler.Arbiter
	// Billing runs price books, billing accounts and invoices (V3 §3);
	// nil disables billing.
	Billing *billing.Service
	// Tokens issues and checks API tokens and device logins; nil disables
	// bearer authentication. TokenRate and OrgTokenRate are requests per
	// minute per token and per organisation's tokens (defaults 600, 1200).
	Tokens *tokens.Service
	// Services is backend services (V4 §2).
	Services     *services.Service
	TokenRate    int
	OrgTokenRate int
	// PublicURL is the UI's address, for device-login links; the request's
	// host when empty.
	PublicURL string
	// Now is the clock (tests); time.Now when nil.
	Now             func() time.Time
	MetricsInterval time.Duration
	MetricsToken    string
}

// NewHandler returns the root HTTP handler: the API under /api, health
// probes at /healthz and /readyz, and the SPA everywhere else.
func NewHandler(opts Options) http.Handler {
	if opts.Auth == nil && !opts.InsecureNoAuth {
		panic("api: Options.Auth is required")
	}
	s := &Server{
		log: opts.Logger, db: opts.DB, dev: opts.DevEndpoints, projects: opts.Projects,
		auth: opts.Auth, sec: opts.Security, settings: opts.Settings, publicIPs: opts.PublicIPs, tls: opts.TLS,
		backups: opts.Backups, nodes: opts.Nodes, console: opts.Console, isochecks: opts.IsoChecks, alerts: opts.Alerts, incidents: opts.Incidents, arbiter: opts.PoolerArbiter, orgs: opts.Orgs, mail: opts.Mail, tenancy: opts.Tenancy, branches: opts.Branches, freetier: opts.FreeTier, support: opts.Support, legal: opts.Legal, capacity: opts.Capacity, costs: opts.Costs, regions: opts.Regions, pgversions: opts.PGVersions, insights: opts.Insights,
		webhooks: opts.Webhooks, jobs: opts.Jobs, outbound: opts.Outbound, billing: opts.Billing,
		metricsInterval: opts.MetricsInterval, metricsToken: opts.MetricsToken,
		tokens: opts.Tokens, services: opts.Services, publicBase: strings.TrimRight(opts.PublicURL, "/"), clock: opts.Now,
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	rate, orgRate := opts.TokenRate, opts.OrgTokenRate
	if rate == 0 {
		rate = DefaultTokenRate
	}
	if orgRate == 0 {
		orgRate = DefaultOrgTokenRate
	}
	s.tokenLimit, s.orgTokenLimit = auth.NewLimiter(rate, time.Minute), auth.NewLimiter(orgRate, time.Minute)
	streamCtx := opts.StreamCtx
	if streamCtx == nil {
		streamCtx = context.Background()
	}
	if opts.DB != nil && opts.Notifier != nil {
		s.streamer = jobs.NewStreamer(streamCtx, opts.DB, opts.Notifier, opts.Logger)
	}
	s.audit = opts.Audit
	if s.audit == nil && opts.DB != nil {
		s.audit = auditlog.New(opts.DB, opts.Logger, auditlog.Config{})
		go s.audit.Run(streamCtx)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(opts.Logger))
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)
	r.Use(s.guard)

	gen.HandlerWithOptions(s, gen.ChiServerOptions{
		BaseRouter: r,
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		},
	})

	spa := SPAHandler(opts.UI, opts.UIIndex)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
			return
		}
		spa.ServeHTTP(w, r)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})
	return r
}

func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}

// GetVersion implements GET /api/v1/version.
func (s *Server) GetVersion(w http.ResponseWriter, _ *http.Request) {
	v := version.Get()
	writeJSON(w, http.StatusOK, gen.Version{
		Version:   v.Version,
		Commit:    v.Commit,
		BuildDate: v.BuildDate,
		GoVersion: v.GoVersion,
	})
}

// GetHealthz implements GET /healthz.
func (s *Server) GetHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, gen.Health{Status: "ok"})
}

// GetReadyz implements GET /readyz: ready when the metadata DB answers.
func (s *Server) GetReadyz(w http.ResponseWriter, r *http.Request) {
	if s.db != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.Ping(ctx); err != nil {
			s.log.Warn("readiness check failed", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, gen.Health{Status: "metadata database unavailable"})
			return
		}
	}
	writeJSON(w, http.StatusOK, gen.Health{Status: "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	// Every 429 is a rate limit that passes: say when to come back, unless
	// the caller already did more precisely.
	if status == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", "60")
	}
	writeJSON(w, status, gen.Error{Code: code, Message: msg})
}

func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(ww, r)
			log.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}

func (s *Server) now() time.Time { return s.clock() }

// publicURL is the UI's base address: the configured one, or the request's.
func (s *Server) publicURL(r *http.Request) string {
	if s.publicBase != "" {
		return s.publicBase
	}
	scheme := "https"
	if r.TLS == nil && !s.sec.SecureCookies {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}
