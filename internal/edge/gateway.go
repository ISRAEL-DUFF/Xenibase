package edge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/jwtes"
)

// Error is the body of every error the edge returns (V4 §3.5).
type Error struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"request_id"`
}

// call is one request's state.
type call struct {
	id    string
	start time.Time
	w     *recorder
	r     *http.Request
	p     *project
	ip    string
	keyID *uuid.UUID
	// publishable: the request came with the publishable key.
	publishable bool
	role        string
	userID      *uuid.UUID
	billed      bool
	// storage marks a file download: its bytes are storage egress, and
	// transforms counts image renders.
	storage    bool
	transforms int64
	// hooks are auth webhook events to send once the auth transaction
	// commits.
	hooks []edgeapi.AuthHook
	// wrote are the relations a data API write changed, for the cache.
	wrote []string
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

func (c *call) json(status int, v any) {
	h := c.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	c.w.WriteHeader(status)
	_ = json.NewEncoder(c.w).Encode(v)
}

func (c *call) fail(status int, code, msg string) {
	c.json(status, map[string]Error{"error": {Code: code, Message: msg, RequestID: c.id}})
}

// ServeHTTP is the gateway.
func (e *Edge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := &call{id: newRequestID(), start: time.Now(), w: &recorder{ResponseWriter: w}, r: r, ip: e.clientIP(r)}
	c.w.Header().Set("X-Request-Id", c.id)
	ref, ok := e.refFromHost(r.Host)
	if !ok {
		if r.URL.Path == "/healthz" {
			if !e.Ready() {
				c.fail(http.StatusServiceUnavailable, "starting", "the edge is loading its configuration")
				return
			}
			c.json(http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		c.fail(http.StatusNotFound, "unknown_host", "this hostname isn't a Xenibase project")
		return
	}
	if !e.Ready() {
		c.w.Header().Set("Retry-After", "2")
		c.fail(http.StatusServiceUnavailable, "starting", "the edge is loading its configuration")
		return
	}
	p := e.lookup(ref)
	if p == nil {
		c.fail(http.StatusNotFound, "project_not_found", "no project with backend services at this hostname")
		return
	}
	c.p = p
	defer e.finish(c)
	if !e.cors(c) {
		return
	}
	switch p.cfg.State {
	case edgeapi.StateSuspended:
		c.fail(http.StatusForbidden, "project_suspended", "this project's organisation is suspended")
		return
	case edgeapi.StatePaused:
		// Only a request with one of the project's live keys wakes it: a
		// project's hostname is public, and resuming costs compute.
		if !liveKey(c) {
			c.w.Header().Set("Retry-After", "10")
			c.fail(http.StatusServiceUnavailable, "project_paused", "the project is paused for inactivity; a request with its API key, or a connection to its database, resumes it")
			return
		}
		e.wake(p.cfg.Ref)
		c.w.Header().Set("Retry-After", "10")
		c.fail(http.StatusServiceUnavailable, "project_resuming", "the project is paused and is resuming; retry shortly")
		return
	}
	if p.cfg.RequestsBlocked && planLimited(r.URL.Path) {
		c.w.Header().Set("Retry-After", strconv.Itoa(untilNextMonth(time.Now())))
		c.fail(http.StatusTooManyRequests, "plan_limit_reached",
			"this project's organisation has used its plan's data API requests for the month; upgrade the plan for more (sign-in keeps working)")
		return
	}
	if e.keylessAuth(c) || e.keylessStorage(c) {
		return
	}
	req, ok := e.authorize(c)
	if !ok {
		return
	}
	c.billed = true
	e.route(c, req)
}

// liveKey reports whether c carries one of its project's live API keys,
// in the apikey header or query parameter. It answers nothing.
func liveKey(c *call) bool {
	key := c.r.Header.Get("apikey")
	if key == "" {
		key = c.r.URL.Query().Get("apikey")
	}
	if key == "" {
		return false
	}
	_, ok := c.p.keys[edgeapi.HashKey(key)]
	return ok
}

// refFromHost is the project reference in host (<ref>.<domain>).
func (e *Edge) refFromHost(host string) (string, bool) {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	ref, ok := strings.CutSuffix(host, "."+e.cfg.Domain)
	if !ok || e.cfg.Domain == "" || ref == "" || strings.Contains(ref, ".") {
		return "", false
	}
	return ref, true
}

func (e *Edge) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	for _, pre := range e.cfg.TrustedProxies {
		if pre.Contains(addr) {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				parts := strings.Split(xff, ",")
				if a, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
					return a.String()
				}
			}
			break
		}
	}
	return addr.String()
}

// originAllowed: an empty list allows any origin (publishable keys are made
// to be embedded); otherwise only those listed.
func (p *project) originAllowed(origin string) bool {
	if len(p.cfg.CORSOrigins) == 0 {
		return true
	}
	for _, o := range p.cfg.CORSOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// corsHeaders are the request headers a browser may always send. x-client-info
// is sent by every Xenibase and Supabase client.
const corsHeaders = "apikey, authorization, content-type, x-request-id, read-replica, prefer, x-upsert, x-metadata, range, if-none-match, cache-control, idempotency-key, x-client-info"

// allowHeaders is the Access-Control-Allow-Headers answer to a preflight that
// asks (Access-Control-Request-Headers) to send the headers in requested: the
// standard list plus each well-formed header name it names. Authentication is
// in explicit headers (apikey, authorization), never in ambient credentials like
// cookies, so a header name grants a page nothing the allowed origin couldn't do
// already; a fixed list only breaks the next client that sends one more
// (x-client-info did, for every browser app).
func allowHeaders(requested string) string {
	have := map[string]bool{}
	for _, h := range strings.Split(corsHeaders, ", ") {
		have[h] = true
	}
	out := corsHeaders
	n := 0
	for _, h := range strings.Split(requested, ",") {
		if n >= 40 {
			break
		}
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || len(h) > 64 || !isHeaderName(h) || have[h] {
			continue
		}
		have[h] = true
		out += ", " + h
		n++
	}
	return out
}

// isHeaderName is whether s is an HTTP header field name (an RFC 9110 token).
func isHeaderName(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return s != ""
}

// cors answers preflights and sets the response headers for browser
// requests; false means the request was answered.
func (e *Edge) cors(c *call) bool {
	origin := c.r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	h := c.w.Header()
	h.Add("Vary", "Origin")
	if !c.p.originAllowed(origin) {
		c.fail(http.StatusForbidden, "origin_not_allowed", "this origin isn't in the project's allowed origins")
		return false
	}
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After, ETag, Content-Range, Content-Length, Content-Disposition, X-Cache, Idempotent-Replayed")
	if c.r.Method == http.MethodOptions && c.r.Header.Get("Access-Control-Request-Method") != "" {
		h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		h.Add("Vary", "Access-Control-Request-Headers")
		h.Set("Access-Control-Allow-Headers", allowHeaders(c.r.Header.Get("Access-Control-Request-Headers")))
		h.Set("Access-Control-Max-Age", "600")
		c.w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

// spendCapRate is a per-minute rate limit under a spend cap (0 stays
// unlimited).
func spendCapRate(perMinute int) int {
	if perMinute <= 0 {
		return perMinute
	}
	return max(perMinute/edgeapi.SpendCapRateDivisor, 1)
}

// authorize checks the API key and the user's token, and the rate limits.
func (e *Edge) authorize(c *call) (Request, bool) {
	p, r := c.p, c.r
	key := r.Header.Get("apikey")
	if key == "" {
		key = r.URL.Query().Get("apikey")
	}
	if key == "" {
		c.fail(http.StatusUnauthorized, "key_required", "send the project's publishable or secret key in the apikey header")
		return Request{}, false
	}
	k, ok := p.keys[edgeapi.HashKey(key)]
	if !ok {
		c.fail(http.StatusUnauthorized, "invalid_key", "this key isn't a live key of this project")
		return Request{}, false
	}
	id := k.ID
	c.keyID = &id
	perIP, perKey, bucket := p.cfg.Settings.RatePerIP, p.cfg.Settings.RatePerKey, ""
	capped := p.cfg.SpendCapped && !strings.HasPrefix(r.URL.Path, "/auth/")
	if capped {
		// Sign-in keeps its limits and its own buckets; everything else
		// slows down (V4 §12).
		perIP, perKey, bucket = spendCapRate(perIP), spendCapRate(perKey), "capped:"
	}
	if !e.limits.allow(bucket+"ip:"+p.cfg.Ref+":"+c.ip, perIP) || !e.limits.allow(bucket+"key:"+k.ID.String(), perKey) {
		c.w.Header().Set("Retry-After", "1")
		if capped {
			c.fail(http.StatusTooManyRequests, "spend_cap_rate_limited", "the organisation has reached its spend cap, so this project's request rate is reduced; slow down or raise the cap")
		} else {
			c.fail(http.StatusTooManyRequests, "rate_limited", "too many requests; slow down")
		}
		return Request{}, false
	}
	req := Request{Timeout: time.Duration(p.cfg.Settings.StatementTimeoutMs) * time.Millisecond}
	if k.Kind == edgeapi.KindSecret {
		if r.Header.Get("Origin") != "" && !p.cfg.Settings.AllowSecretInBrowser {
			c.fail(http.StatusForbidden, "secret_key_in_browser", "a secret key can't be used from a browser; use the publishable key")
			return Request{}, false
		}
		req.Role, req.Claims = "service", map[string]any{"role": "service"}
		c.role = req.Role
		return req, true
	}
	c.publishable = true
	req.Role, req.Claims = "anon", map[string]any{"role": "anon"}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && strings.TrimSpace(tok) != "" {
		claims, err := jwtes.Verify(strings.TrimSpace(tok), p.jwks, p.cfg.Ref, time.Now())
		if err != nil {
			c.fail(http.StatusUnauthorized, "invalid_token", err.Error())
			return Request{}, false
		}
		if claims["role"] != "user" {
			c.fail(http.StatusUnauthorized, "invalid_token", "the token isn't a user's access token")
			return Request{}, false
		}
		if sub, _ := claims["sub"].(string); sub != "" {
			if u, err := uuid.Parse(sub); err == nil {
				c.userID = &u
			}
		}
		req.Role, req.Claims = "user", claims
	}
	c.role = req.Role
	return req, true
}

// route sends a request to its service.
func (e *Edge) route(c *call, req Request) {
	path := c.r.URL.Path
	if strings.HasPrefix(path, "/data/v1/") {
		// Only the data API reads from replicas: auth and storage GETs
		// can write (a verification link, a callback).
		req.Replica = c.replicaRead()
	}
	switch {
	case path == "/data/v1/health" && (c.r.Method == http.MethodGet || c.r.Method == http.MethodHead):
		e.health(c, req)
	case strings.HasPrefix(path, "/data/v1/"):
		if mfaRequired(c.p.cfg.Auth, req) {
			c.fail(http.StatusForbidden, "mfa_required", "this project requires a second factor: verify one to reach aal2")
			return
		}
		e.data(c, req)
	case strings.HasPrefix(path, "/auth/v1/"):
		e.auth(c, req)
	case strings.HasPrefix(path, "/storage/v1/"):
		e.storageRoute(c, req)
	case strings.HasPrefix(path, "/realtime/v1"):
		if mfaRequired(c.p.cfg.Auth, req) {
			c.fail(http.StatusForbidden, "mfa_required", "this project requires a second factor: verify one to reach aal2")
			return
		}
		e.realtime(c, req)
	default:
		c.fail(http.StatusNotFound, "no_such_endpoint", "no such endpoint")
	}
}

// health runs a request transaction and reports who it ran as: the edge,
// the pooler, the project database, and the role and claims, end to end.
func (e *Edge) health(c *call, req Request) {
	ctx, cancel := context.WithTimeout(c.r.Context(), req.Timeout+2*time.Second)
	defer cancel()
	var role, dbRole string
	var uid *string
	var replica bool // served by a read replica
	err := e.WithRequest(ctx, c.p, req, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(xb_auth.role(), ''), xb_auth.uid()::text, current_user, pg_is_in_recovery()`).Scan(&role, &uid, &dbRole, &replica)
	})
	if err != nil {
		e.dbError(c, err)
		return
	}
	out := map[string]any{"status": "ok", "project": c.p.cfg.Ref, "role": role, "region": c.p.cfg.Region, "replica": replica}
	if uid != nil {
		out["user_id"] = *uid
	}
	c.json(http.StatusOK, out)
}

// dbError maps a database failure to a response.
func (e *Edge) dbError(c *call, err error) {
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, errConfigChanged):
		c.w.Header().Set("Retry-After", "1")
		c.fail(http.StatusServiceUnavailable, "retry", err.Error())
	case errors.As(err, &pe) && pe.Code == "57014":
		c.fail(http.StatusGatewayTimeout, "statement_timeout", "the query took longer than the project's limit")
	case errors.As(err, &pe) && unavailable(pe.Code):
		e.cfg.Log.Warn("edge database", "project", c.p.cfg.Ref, "err", err)
		c.w.Header().Set("Retry-After", "5")
		c.fail(http.StatusServiceUnavailable, "database_unavailable", "the project's database can't be reached right now")
	case errors.As(err, &pe):
		c.json(http.StatusBadRequest, map[string]Error{"error": {Code: "database_error", Message: pe.Message,
			Details: map[string]any{"pg_code": pe.Code}, RequestID: c.id}})
	default:
		e.cfg.Log.Warn("edge database", "project", c.p.cfg.Ref, "err", err)
		c.w.Header().Set("Retry-After", "5")
		c.fail(http.StatusServiceUnavailable, "database_unavailable", "the project's database can't be reached right now")
	}
}

// unavailable: connection, authentication and resource errors (and an
// administrator's shutdown) are the database not being reachable, not the
// request's fault: retry later.
func unavailable(code string) bool {
	return strings.HasPrefix(code, "08") || strings.HasPrefix(code, "28") || strings.HasPrefix(code, "53") ||
		strings.HasPrefix(code, "57P")
}

// finish meters and logs a request that reached a project.
func (e *Edge) finish(c *call) {
	status := c.w.status
	if status == 0 {
		status = http.StatusOK
	}
	l := edgeapi.Log{ProjectID: c.p.cfg.ProjectID, At: c.start.UTC(), RequestID: c.id, Method: c.r.Method,
		Path: c.r.URL.Path, Status: status, LatencyMs: int(time.Since(c.start).Milliseconds()), Role: c.role,
		UserID: c.userID, KeyID: c.keyID, IP: c.ip, BytesOut: c.w.bytes}
	e.meter.record(l, c.billed, c.storage, c.transforms)
}

// wake asks xenibase-server to resume a paused project, at most every 30
// seconds per project.
func (e *Edge) wake(ref string) {
	now := time.Now()
	if last, ok := e.waking.Load(ref); ok && now.Sub(last.(time.Time)) < 30*time.Second {
		return
	}
	e.waking.Store(ref, now)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := e.client.Wake(ctx, ref); err != nil {
			e.cfg.Log.Warn("edge wake", "project", ref, "err", err)
		}
	}()
}

// replicaRead reports whether a request may read from the project's read
// replicas (V4 §7): a GET or HEAD, when the project has some, that asks
// with Read-Replica: allowed, or comes with the publishable key of a
// project that sends those by default. Replicas trail the primary by up
// to the lag threshold, so a client that must read its own writes leaves
// the header off.
func (c *call) replicaRead() bool {
	if c.p.cfg.ReadDatabase == "" || (c.r.Method != http.MethodGet && c.r.Method != http.MethodHead) {
		return false
	}
	switch strings.ToLower(c.r.Header.Get("Read-Replica")) {
	case "allowed":
		return true
	case "", "default":
		return c.publishable && c.p.cfg.Settings.ReplicaReads
	}
	return false // "primary", or anything else
}

// planLimited: what a plan's monthly request limit stops (V4.1 §3). Sign-in
// and the health check carry on.
func planLimited(path string) bool {
	return !strings.HasPrefix(path, "/auth/") && path != "/data/v1/health"
}

// untilNextMonth is the seconds to the start of the next month (UTC).
func untilNextMonth(now time.Time) int {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	return max(int(next.Sub(now).Seconds()), 1)
}
