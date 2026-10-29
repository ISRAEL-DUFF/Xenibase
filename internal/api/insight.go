package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/console"
	"github.com/israel-duff/xenibase/internal/metrics"
	"github.com/israel-duff/xenibase/internal/provision"
)

// haveConsole reports whether the console service (also extensions) runs.
func (s *Server) haveConsole(w http.ResponseWriter) bool {
	if !s.requireProjects(w) {
		return false
	}
	if s.console == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the SQL console is not configured")
		return false
	}
	return true
}

// requireConsole also refuses when the console is turned off.
func (s *Server) requireConsole(w http.ResponseWriter) bool {
	if !s.haveConsole(w) {
		return false
	}
	if s.console.Disabled() {
		writeError(w, http.StatusForbidden, "console_disabled", console.ErrDisabled.Error())
		return false
	}
	return true
}

func (s *Server) consoleError(w http.ResponseWriter, what string, err error) {
	var asleep *console.AsleepError
	switch {
	case errors.As(err, &asleep):
		s.writeAsleep(context.Background(), w, asleep.ProjectID, asleep.Lifecycle)
	case errors.Is(err, console.ErrDisabled):
		writeError(w, http.StatusForbidden, "console_disabled", err.Error())
	case errors.Is(err, console.ErrNotActive):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, console.ErrNoTable):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, console.ErrBadCursor), errors.Is(err, console.ErrNotAllowed):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, console.ErrDenied):
		writeError(w, http.StatusForbidden, "permission_denied", err.Error())
	default:
		s.provisionError(w, what, err)
	}
}

// RunSQL implements POST /api/v1/projects/{id}/sql.
func (s *Server) RunSQL(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	a := auditFrom(r.Context())
	a.target("project", id.String())
	if !s.requireConsole(w) {
		return
	}
	var req gen.SqlRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, console.MaxQueryBytes+4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "query is empty")
		return
	}
	if req.QueryId == uuid.Nil {
		writeError(w, http.StatusBadRequest, "bad_request", "query_id is required")
		return
	}
	if len(req.Query) > console.MaxQueryBytes {
		writeError(w, http.StatusBadRequest, "bad_request", "query is too long")
		return
	}
	cr := console.Request{Query: req.Query, QueryID: req.QueryId, ReadOnly: req.ReadOnly != nil && *req.ReadOnly}
	// Read-only members run as the project's read-only role (V2 §2.3).
	if ok, err := s.can(r.Context(), authz.ConsoleWrite); err != nil {
		s.internalError(w, "run sql", err)
		return
	} else if !ok {
		if !cr.ReadOnly {
			if sess, _ := sessionFrom(r.Context()); sess.Token != nil && !authz.HasScope(sess.Token.Scopes, authz.ScopeWrite) {
				writeError(w, http.StatusForbidden, "insufficient_scope", "this token has the read scope only: send read_only, or use a token with the write scope")
				return
			}
			writeError(w, http.StatusForbidden, "forbidden", "your project role is read-only: turn on read-only mode")
			return
		}
		cr.AsReadOnlyRole = true
	}
	if t := req.TimeoutSeconds; t != nil {
		if *t < 1 || time.Duration(*t)*time.Second > console.MaxTimeout {
			writeError(w, http.StatusBadRequest, "bad_request", "timeout_seconds must be between 1 and 300")
			return
		}
		cr.Timeout = time.Duration(*t) * time.Second
	}
	if s.tenancy != nil {
		// Concurrent console queries per organisation (V2 §10.3).
		release, err := s.tenancy.AcquireConsole(r.Context(), accessFrom(r.Context()).OrgID)
		if err != nil {
			s.tenancyError(w, "run sql", err)
			return
		}
		defer release()
	}
	out, err := s.console.Run(r.Context(), id, cr)
	if err != nil {
		s.consoleError(w, "run sql", err)
		return
	}
	// The audit entry records that the console was used, never the SQL.
	a.set("read_only", out.ReadOnly)
	a.set("statements", len(out.Results))
	if out.Error != nil {
		a.set("sql_error", out.Error.Code)
	}
	writeJSON(w, http.StatusOK, out)
}

// CancelSQL implements POST /api/v1/projects/{id}/sql/cancel.
func (s *Server) CancelSQL(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	auditFrom(r.Context()).target("project", id.String())
	if !s.requireConsole(w) {
		return
	}
	var req gen.SqlCancelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ok, err := s.console.Cancel(r.Context(), id, req.QueryId)
	if err != nil {
		s.consoleError(w, "cancel sql", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.SqlCancelResult{Cancelled: ok})
}

// GetProjectSchema implements GET /api/v1/projects/{id}/schema.
func (s *Server) GetProjectSchema(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireConsole(w) {
		return
	}
	sc, err := s.console.Schema(r.Context(), id)
	if err != nil {
		s.consoleError(w, "schema", err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func toAPIExtensions(exts []console.Extension) gen.ExtensionList {
	out := gen.ExtensionList{Items: make([]gen.Extension, 0, len(exts))}
	for _, e := range exts {
		out.Items = append(out.Items, gen.Extension{
			Name: e.Name, Tier: gen.ExtensionTier(e.Tier), Allowed: e.Allowed, Available: e.Available,
			DefaultVersion: e.DefaultVersion, InstalledVersion: e.InstalledVersion, Schema: e.Schema,
		})
	}
	return out
}

// ListProjectExtensions implements GET /api/v1/projects/{id}/extensions.
func (s *Server) ListProjectExtensions(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.haveConsole(w) {
		return
	}
	exts, err := s.console.Extensions(r.Context(), id)
	if err != nil {
		s.consoleError(w, "list extensions", err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIExtensions(exts))
}

// EnableProjectExtension implements POST /api/v1/projects/{id}/extensions.
func (s *Server) EnableProjectExtension(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	a := auditFrom(r.Context())
	a.target("project", id.String())
	if !s.haveConsole(w) {
		return
	}
	var req gen.EnableExtensionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a.set("extension", req.Name)
	exts, err := s.console.EnableExtension(r.Context(), id, req.Name)
	if err != nil {
		s.consoleError(w, "enable extension", err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIExtensions(exts))
}

func metricQuery(w http.ResponseWriter, rng *string, names *gen.MetricNames) (string, []string, bool) {
	r := "24h"
	if rng != nil {
		r = *rng
	}
	if _, ok := metrics.Ranges[r]; !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "range must be 1h, 24h, or 7d")
		return "", nil, false
	}
	var ms []string
	if names != nil {
		ms = *names
	}
	return r, ms, true
}

func toAPISeries(rng string, series []metrics.Series) gen.MetricsResponse {
	out := gen.MetricsResponse{Range: rng, Resolution: gen.MetricsResponseResolution(metrics.Resolution(rng)), Series: []gen.MetricSeries{}}
	for _, se := range series {
		ms := gen.MetricSeries{Metric: se.Metric, Points: make([]gen.MetricPoint, 0, len(se.Points))}
		for _, p := range se.Points {
			ms.Points = append(ms.Points, gen.MetricPoint{Ts: p.TS, Value: p.Value})
		}
		out.Series = append(out.Series, ms)
	}
	return out
}

// GetProjectMetrics implements GET /api/v1/projects/{id}/metrics.
func (s *Server) GetProjectMetrics(w http.ResponseWriter, r *http.Request, id gen.ProjectID, params gen.GetProjectMetricsParams) {
	if !s.requireProjects(w) || s.db == nil {
		return
	}
	rng, names, ok := metricQuery(w, (*string)(params.Range), params.Metric)
	if !ok {
		return
	}
	p, err := s.tenantProjectLive(r.Context())
	if err != nil {
		s.provisionError(w, "project metrics", err)
		return
	}
	series, err := metrics.Query(r.Context(), s.db, metrics.ScopeProject, id, rng, names)
	if err != nil {
		s.internalError(w, "project metrics", err)
		return
	}
	out := toAPISeries(rng, series)
	if p.Status == provision.StatusActive {
		avail, top, err := metrics.TopQueries(r.Context(), s.projects, p)
		if err != nil {
			s.log.Warn("top queries", "project_id", id, "err", err)
		} else {
			tq := &gen.TopQueries{Available: avail, Items: make([]gen.TopQuery, 0, len(top))}
			for _, t := range top {
				tq.Items = append(tq.Items, gen.TopQuery{Query: t.Query, Calls: t.Calls, TotalMs: t.TotalMS, MeanMs: t.MeanMS, Rows: t.Rows})
			}
			out.TopQueries = tq
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// GetNodeMetrics implements GET /api/v1/nodes/{id}/metrics.
func (s *Server) GetNodeMetrics(w http.ResponseWriter, r *http.Request, id gen.NodeID, params gen.GetNodeMetricsParams) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "metrics are not configured")
		return
	}
	rng, names, ok := metricQuery(w, (*string)(params.Range), params.Metric)
	if !ok {
		return
	}
	series, err := metrics.Query(r.Context(), s.db, metrics.ScopeNode, id, rng, names)
	if err != nil {
		s.internalError(w, "node metrics", err)
		return
	}
	writeJSON(w, http.StatusOK, toAPISeries(rng, series))
}

// GetPrometheusMetrics implements GET /metrics: a bearer token
// (XENIBASE_METRICS_TOKEN) or a signed-in operator.
func (s *Server) GetPrometheusMetrics(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "metrics are not configured")
		return
	}
	if !s.metricsAuthorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="xenibase"`)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "a metrics token or a platform admin session is required")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := metrics.WritePrometheus(r.Context(), w, s.db, s.metricsInterval); err != nil {
		s.log.Error("prometheus metrics", "err", err)
	}
	// This process's own counters: they matter most while the database
	// is refusing writes.
	if s.audit != nil {
		_ = s.audit.WritePrometheus(w)
	}
}

func (s *Server) metricsAuthorized(r *http.Request) bool {
	if s.auth == nil {
		return true // unit tests without auth
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && s.metricsToken != "" {
		return subtle.ConstantTimeCompare([]byte(tok), []byte(s.metricsToken)) == 1
	}
	c, err := r.Cookie(s.cookieName(sessionName))
	if err != nil {
		return false
	}
	// Every project's metrics: the platform admin's alone.
	sess, err := s.auth.Authenticate(r.Context(), c.Value)
	return err == nil && sess.PlatformAdmin()
}
