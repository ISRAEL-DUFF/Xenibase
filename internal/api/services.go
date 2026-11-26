package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/services"
	"github.com/israel-duff/xenibase/internal/store"
)

// Backend services (V4 §2): the dashboard's management endpoints, and the
// signed feed xenibase-edge follows.

func (s *Server) requireServices(w http.ResponseWriter) bool {
	if s.services == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "backend services aren't available on this install")
		return false
	}
	return true
}

func (s *Server) servicesError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, services.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, services.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, services.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, services.ErrEdgeScope):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	default:
		s.internalError(w, what, err)
	}
}

func apiKeyOut(k store.ProjectApiKey) gen.ApiKey {
	return gen.ApiKey{Id: k.ID, Kind: gen.ApiKeyKind(k.Kind), Name: k.Name, Prefix: k.Prefix, Key: k.Display,
		LastUsedAt: k.LastUsedAt, RevokedAt: k.RevokedAt, CreatedAt: k.CreatedAt}
}

func (s *Server) backendServicesOut(r *http.Request, p store.Project, svc *store.ProjectService) (gen.BackendServices, error) {
	out := gen.BackendServices{Keys: []gen.ApiKey{}, CorsOrigins: []string{}, ExposedSchemas: []string{"public"}, PublicTables: []string{},
		FeedConfigured: s.services.FeedEnabled()}
	if svc == nil {
		return out, nil
	}
	st, err := services.DecodeSettings(svc.Settings)
	if err != nil {
		return out, err
	}
	ref, url := svc.Ref, s.services.URL(svc.Ref, p.Region)
	out.Enabled, out.Ref, out.Url, out.EnabledAt, out.CorsOrigins = svc.Enabled, &ref, &url, svc.EnabledAt, svc.CorsOrigins
	out.ExposedSchemas, out.PublicTables = svc.ExposedSchemas, svc.PublicTables
	anon, user, service := store.AnonRole(p.DbName), store.UserRole(p.DbName), store.ServiceRole(p.DbName)
	out.Roles = &struct {
		Anon    *string `json:"anon,omitempty"`
		Service *string `json:"service,omitempty"`
		User    *string `json:"user,omitempty"`
	}{Anon: &anon, User: &user, Service: &service}
	out.Settings = gen.BackendServicesSettings{StatementTimeoutMs: &st.StatementTimeoutMs, RatePerIp: &st.RatePerIP,
		RatePerKey: &st.RatePerKey, AllowSecretInBrowser: &st.AllowSecretInBrowser, MaxQueryCost: &st.MaxQueryCost,
		ReplicaReads: &st.ReplicaReads, CacheTtlSeconds: &st.CacheTTLSeconds}
	if st.CacheTTLSeconds == nil {
		empty := map[string]int{}
		out.Settings.CacheTtlSeconds = &empty
	}
	timeout, perIP, perKey := services.EffectiveSettings(st, services.PlanCeilings{TimeoutMs: svc.PlanTimeoutMs,
		RatePerIP: svc.PlanRatePerIp, RatePerKey: svc.PlanRatePerKey})
	eff := gen.BackendServicesEffective{StatementTimeoutMs: timeout, RatePerIp: perIP, RatePerKey: perKey,
		RequestsBlocked: svc.ApiRequestsBlocked, MauBlocked: svc.MauBlocked,
		PlanTimeoutMs: intPtr32(svc.PlanTimeoutMs), PlanRatePerIp: intPtr32(svc.PlanRatePerIp), PlanRatePerKey: intPtr32(svc.PlanRatePerKey)}
	if from, err := s.services.PlanLimitsFrom(r.Context()); err == nil && !from.IsZero() {
		eff.LimitsFrom = &from
	}
	out.Effective = &eff
	keys, err := store.New(s.db).ListAPIKeys(r.Context(), p.ID)
	if err != nil {
		return out, err
	}
	for _, k := range keys {
		out.Keys = append(out.Keys, apiKeyOut(k))
	}
	return out, nil
}

// GetBackendServices implements GET /api/v1/projects/{id}/services.
func (s *Server) GetBackendServices(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	q := store.New(s.db)
	p, err := q.GetProject(r.Context(), id)
	if err != nil {
		s.provisionError(w, "backend services", err)
		return
	}
	var svc *store.ProjectService
	if row, err := q.GetProjectServices(r.Context(), id); err == nil {
		svc = &row
	} else if !errors.Is(err, pgx.ErrNoRows) {
		s.internalError(w, "backend services", err)
		return
	}
	out, err := s.backendServicesOut(r, p, svc)
	if err != nil {
		s.internalError(w, "backend services", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// EnableBackendServices implements POST /api/v1/projects/{id}/services.
func (s *Server) EnableBackendServices(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	auditFrom(r.Context()).target("project", id.String())
	en, err := s.services.Enable(r.Context(), id, userID(r.Context()))
	if err != nil {
		s.servicesError(w, "enable backend services", err)
		return
	}
	p, err := store.New(s.db).GetProject(r.Context(), id)
	if err != nil {
		s.internalError(w, "enable backend services", err)
		return
	}
	svc, err := s.backendServicesOut(r, p, &en.Services)
	if err != nil {
		s.internalError(w, "enable backend services", err)
		return
	}
	op, err := toAPIOperation(en.Operation)
	if err != nil {
		s.internalError(w, "enable backend services", err)
		return
	}
	out := gen.BackendServicesEnabled{Services: svc, Operation: op, Keys: []gen.CreatedApiKey{}}
	for _, k := range en.Keys {
		out.Keys = append(out.Keys, gen.CreatedApiKey{Key: apiKeyOut(k.ProjectApiKey), Value: k.Key})
	}
	w.Header().Set("Location", "/api/v1/operations/"+en.Operation.ID.String())
	writeJSON(w, http.StatusAccepted, out)
}

// UpdateBackendServices implements PATCH /api/v1/projects/{id}/services.
func (s *Server) UpdateBackendServices(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	var req gen.BackendServicesUpdate
	if !decodeJSON(w, r, &req) {
		return
	}
	q := store.New(s.db)
	cur, err := q.GetProjectServices(r.Context(), id)
	if err != nil {
		s.servicesError(w, "backend services", err)
		return
	}
	st, err := services.DecodeSettings(cur.Settings)
	if err != nil {
		s.internalError(w, "backend services", err)
		return
	}
	origins := cur.CorsOrigins
	if req.CorsOrigins != nil {
		origins = *req.CorsOrigins
	}
	if v := req.Settings; v != nil {
		if v.StatementTimeoutMs != nil {
			st.StatementTimeoutMs = *v.StatementTimeoutMs
		}
		if v.RatePerIp != nil {
			st.RatePerIP = *v.RatePerIp
		}
		if v.RatePerKey != nil {
			st.RatePerKey = *v.RatePerKey
		}
		if v.AllowSecretInBrowser != nil {
			st.AllowSecretInBrowser = *v.AllowSecretInBrowser
		}
		if v.MaxQueryCost != nil {
			st.MaxQueryCost = *v.MaxQueryCost
		}
		if v.ReplicaReads != nil {
			st.ReplicaReads = *v.ReplicaReads
		}
		if v.CacheTtlSeconds != nil {
			st.CacheTTLSeconds = *v.CacheTtlSeconds
			if len(st.CacheTTLSeconds) == 0 {
				st.CacheTTLSeconds = nil
			}
		}
	}
	exposed, public := cur.ExposedSchemas, cur.PublicTables
	if req.ExposedSchemas != nil {
		exposed = *req.ExposedSchemas
	}
	if req.PublicTables != nil {
		public = *req.PublicTables
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("cors_origins", origins)
	a.set("settings", st)
	a.set("exposed_schemas", exposed)
	a.set("public_tables", public)
	if _, err := s.services.UpdateExposure(r.Context(), id, origins, st, exposed, public); err != nil {
		s.servicesError(w, "backend services", err)
		return
	}
	s.GetBackendServices(w, r, id)
}

// DisableBackendServices implements DELETE /api/v1/projects/{id}/services.
func (s *Server) DisableBackendServices(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	auditFrom(r.Context()).target("project", id.String())
	op, err := s.services.Disable(r.Context(), id, userID(r.Context()))
	if err != nil {
		s.servicesError(w, "disable backend services", err)
		return
	}
	s.writeOperation(w, "disable backend services", op)
}

// CreateAPIKey implements POST /api/v1/projects/{id}/services/keys.
func (s *Server) CreateAPIKey(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	var req gen.CreateApiKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("kind", string(req.Kind))
	a.set("name", req.Name)
	k, err := s.services.CreateKey(r.Context(), id, string(req.Kind), req.Name, userID(r.Context()))
	if err != nil {
		s.servicesError(w, "create API key", err)
		return
	}
	a.set("key_id", k.ID.String())
	writeJSON(w, http.StatusCreated, gen.CreatedApiKey{Key: apiKeyOut(k.ProjectApiKey), Value: k.Key})
}

// RevokeAPIKey implements DELETE /api/v1/projects/{id}/services/keys/{key_id}.
func (s *Server) RevokeAPIKey(w http.ResponseWriter, r *http.Request, id gen.ProjectID, keyID openapi_types.UUID) {
	if !s.requireServices(w) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("key_id", keyID.String())
	k, err := s.services.RevokeKey(r.Context(), id, keyID)
	if err != nil {
		s.servicesError(w, "revoke API key", err)
		return
	}
	writeJSON(w, http.StatusOK, apiKeyOut(k))
}

// ListAPIRequestLogs implements GET /api/v1/projects/{id}/services/logs.
func (s *Server) ListAPIRequestLogs(w http.ResponseWriter, r *http.Request, id gen.ProjectID, params gen.ListAPIRequestLogsParams) {
	lim := 100
	if params.Limit != nil {
		lim = *params.Limit
	}
	if lim < 1 || lim > 500 {
		writeError(w, http.StatusBadRequest, "bad_request", "limit must be 1 to 500")
		return
	}
	smin, smax := 0, 999
	if params.Status != nil {
		st := *params.Status
		if n, err := strconv.Atoi(st); err == nil {
			smin, smax = n, n
		} else if len(st) == 3 && st[1:] == "xx" && st[0] >= '1' && st[0] <= '5' {
			smin = int(st[0]-'0') * 100
			smax = smin + 99
		} else {
			writeError(w, http.StatusBadRequest, "bad_request", "status is a code (404) or a class (5xx)")
			return
		}
	}
	prefix := ""
	if params.Path != nil {
		prefix = strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(*params.Path)
	}
	q := store.New(s.db)
	toAPI := func(rows []store.ApiRequestLog) []gen.ApiRequestLog {
		out := []gen.ApiRequestLog{}
		for _, l := range rows {
			out = append(out, gen.ApiRequestLog{Id: l.ID, At: l.At, RequestId: l.RequestID, Method: l.Method, Path: l.Path,
				Status: int(l.Status), LatencyMs: int(l.LatencyMs), Role: l.Role, UserId: l.UserID, KeyId: l.KeyID, Ip: l.Ip, BytesOut: l.BytesOut})
		}
		return out
	}
	if params.After == nil {
		rows, err := q.ProjectRequestLogs(r.Context(), store.ProjectRequestLogsParams{ProjectID: id, Before: params.Before, Lim: int32(lim),
			StatusMin: int32(smin), StatusMax: int32(smax), PathPrefix: prefix})
		if err != nil {
			s.internalError(w, "API request logs", err)
			return
		}
		writeJSON(w, http.StatusOK, gen.ApiRequestLogList{Items: toAPI(rows)})
		return
	}
	// Following: after 0 starts from now.
	after := *params.After
	if after <= 0 {
		latest, err := q.LatestRequestLogID(r.Context(), id)
		if err != nil {
			s.internalError(w, "API request logs", err)
			return
		}
		writeJSON(w, http.StatusOK, gen.ApiRequestLogList{Items: []gen.ApiRequestLog{}, Next: &latest})
		return
	}
	wait := 0
	if params.Wait != nil {
		wait = min(max(*params.Wait, 0), 25)
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		rows, err := q.ProjectRequestLogsAfter(r.Context(), store.ProjectRequestLogsAfterParams{ProjectID: id, After: after, Lim: int32(lim),
			StatusMin: int32(smin), StatusMax: int32(smax), PathPrefix: prefix})
		if err != nil {
			s.internalError(w, "API request logs", err)
			return
		}
		next := after
		if len(rows) > 0 {
			next = rows[len(rows)-1].ID
		}
		if len(rows) > 0 || !time.Now().Before(deadline) {
			// A filtered follow moves the cursor past logs it skipped too.
			if latest, err := q.LatestRequestLogID(r.Context(), id); err == nil && len(rows) == 0 && latest > next {
				next = latest
			}
			writeJSON(w, http.StatusOK, gen.ApiRequestLogList{Items: toAPI(rows), Next: &next})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// ---- xenibase-edge's feed ------------------------------------------------------

// edgeAuth reads the body, checks the edge signature, and returns which
// regions the edge may see.
func (s *Server) edgeAuth(w http.ResponseWriter, r *http.Request) ([]byte, services.EdgeScope, bool) {
	if s.services == nil || !s.services.FeedEnabled() {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return nil, services.EdgeScope{}, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "unreadable body")
		return nil, services.EdgeScope{}, false
	}
	scope, err := s.services.VerifyEdge(r, body)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "bad edge signature")
		return nil, services.EdgeScope{}, false
	}
	return body, scope, true
}

// edgeRef checks that the project an edge request is about is in the
// edge's scope.
func (s *Server) edgeRef(w http.ResponseWriter, r *http.Request, scope services.EdgeScope, ref, what string) bool {
	if err := s.services.EdgeProjectInScope(r.Context(), scope, ref); err != nil {
		s.servicesError(w, what, err)
		return false
	}
	return true
}

// EdgeConfig implements GET /api/v1/edge/config.
func (s *Server) EdgeConfig(w http.ResponseWriter, r *http.Request, params gen.EdgeConfigParams) {
	_, scope, ok := s.edgeAuth(w, r)
	if !ok {
		return
	}
	var since int64
	if params.Since != nil {
		since = *params.Since
	}
	wait := 0
	if params.Wait != nil {
		wait = min(max(*params.Wait, 0), 30)
	}
	region := ""
	if params.Region != nil {
		region = *params.Region
	}
	allow, err := scope.Feed(region)
	if err != nil {
		s.servicesError(w, "edge config", err)
		return
	}
	c, err := s.services.Config(r.Context(), allow, since, time.Duration(wait)*time.Second)
	if err != nil {
		s.internalError(w, "edge config", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// EdgeReport implements POST /api/v1/edge/report.
func (s *Server) EdgeReport(w http.ResponseWriter, r *http.Request) {
	body, scope, ok := s.edgeAuth(w, r)
	if !ok {
		return
	}
	var rep edgeapi.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "not a report")
		return
	}
	if err := s.services.ScopeReport(r.Context(), scope, &rep); err != nil {
		s.servicesError(w, "edge report", err)
		return
	}
	if err := s.services.Report(r.Context(), rep); err != nil {
		s.servicesError(w, "edge report", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// EdgeWake implements POST /api/v1/edge/wake.
func (s *Server) EdgeWake(w http.ResponseWriter, r *http.Request) {
	body, scope, ok := s.edgeAuth(w, r)
	if !ok {
		return
	}
	var req edgeapi.Wake
	if err := json.Unmarshal(body, &req); err != nil || req.Ref == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "not a wake request")
		return
	}
	if !s.edgeRef(w, r, scope, req.Ref, "edge wake") {
		return
	}
	if err := s.services.Wake(r.Context(), req.Ref); err != nil {
		s.servicesError(w, "edge wake", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetServiceTypes implements GET /api/v1/projects/{id}/services/types.
func (s *Server) GetServiceTypes(w http.ResponseWriter, r *http.Request, id gen.ProjectID, params gen.GetServiceTypesParams) {
	if !s.requireServices(w) {
		return
	}
	p, err := store.New(s.db).GetProject(r.Context(), id)
	if err != nil {
		s.provisionError(w, "types", err)
		return
	}
	pkg := ""
	if params.Package != nil {
		pkg = *params.Package
		if !goPackageRe.MatchString(pkg) {
			writeError(w, http.StatusBadRequest, "bad_request", "package is a Go package name")
			return
		}
	}
	src, err := s.services.Types(r.Context(), p, string(params.Lang), pkg)
	if err != nil {
		s.servicesError(w, "types", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, src)
}

var goPackageRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// GetSecurityAdvisor implements GET /api/v1/projects/{id}/services/advisor.
func (s *Server) GetSecurityAdvisor(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	p, err := store.New(s.db).GetProject(r.Context(), id)
	if err != nil {
		s.provisionError(w, "advisor", err)
		return
	}
	fs, err := s.services.Advisor(r.Context(), p)
	if err != nil {
		s.servicesError(w, "advisor", err)
		return
	}
	out := gen.AdvisorFindings{Items: []gen.AdvisorFinding{}}
	for _, f := range fs {
		g := gen.AdvisorFinding{Level: gen.AdvisorFindingLevel(f.Level), Code: f.Code, Object: f.Object, Message: f.Message}
		if f.Fix != "" {
			fix := f.Fix
			g.Fix = &fix
		}
		out.Items = append(out.Items, g)
	}
	writeJSON(w, http.StatusOK, out)
}

// ExploreDataAPI implements POST /api/v1/projects/{id}/services/explore.
func (s *Server) ExploreDataAPI(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	var req gen.ExploreRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("method", string(req.Method))
	a.set("path", req.Path)
	a.set("role", string(req.Role))
	var body []byte
	if req.Body != nil {
		body = []byte(*req.Body)
	}
	res, err := s.services.Explore(r.Context(), id, string(req.Role), req.UserId, string(req.Method), req.Path, body)
	if err != nil {
		s.servicesError(w, "explore", err)
		return
	}
	ct := res.ContentType
	writeJSON(w, http.StatusOK, gen.ExploreResponse{Status: res.Status, ContentType: &ct, Body: res.Body})
}

// MigrateSupabase implements POST /api/v1/projects/{id}/migrate/supabase.
func (s *Server) MigrateSupabase(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	var req gen.SupabaseMigrationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("step", string(req.Step))
	m := services.SupabaseMigration{Step: string(req.Step)}
	if req.SourceUrl != nil {
		m.SourceURL = *req.SourceUrl
	}
	if req.S3 != nil {
		m.S3 = &services.SupabaseS3{Endpoint: req.S3.Endpoint, AccessKey: req.S3.AccessKey, SecretKey: req.S3.SecretKey}
		if req.S3.Region != nil {
			m.S3.Region = *req.S3.Region
		}
		a.set("s3_endpoint", req.S3.Endpoint)
	}
	op, err := s.services.MigrateSupabase(r.Context(), id, m, userID(r.Context()))
	if err != nil {
		s.servicesError(w, "supabase migration", err)
		return
	}
	s.writeOperation(w, "supabase migration", op)
}

func intPtr32(v *int32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

// GetServicesCatalog implements GET /api/v1/projects/{id}/services/catalog:
// the API docs' source (V4.1 §9.2) and `xenibase policies list`.
func (s *Server) GetServicesCatalog(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	p, err := store.New(s.db).GetProject(r.Context(), id)
	if err != nil {
		s.provisionError(w, "catalog", err)
		return
	}
	d, err := s.services.Describe(r.Context(), p)
	if err != nil {
		s.servicesError(w, "catalog", err)
		return
	}
	out := gen.ServicesCatalog{Schemas: d.Schemas, Tables: []gen.CatalogTable{}, Functions: []gen.CatalogFunction{}}
	if d.Ref != "" {
		ref, u := d.Ref, s.services.URL(d.Ref, p.Region)
		out.Ref, out.ApiUrl = &ref, &u
	}
	for _, t := range d.Tables {
		ct := gen.CatalogTable{Schema: t.Schema, Name: t.Name, Kind: gen.CatalogTableKind(t.Kind), Rls: t.RLS, Public: t.Public,
			PrimaryKey: t.PrimaryKey, Columns: []gen.CatalogColumn{}, ForeignKeys: []gen.CatalogForeignKey{}, ReferencedBy: []gen.CatalogForeignKey{},
			Policies: []gen.CatalogPolicy{}, Access: map[string]gen.CatalogAccess{}}
		for _, c := range t.Columns {
			cc := gen.CatalogColumn{Name: c.Name, Type: c.Type, Nullable: c.Nullable, Identity: c.Identity, Generated: c.Generated}
			if c.Default != "" {
				def := c.Default
				cc.Default = &def
			}
			if len(c.Enum) > 0 {
				e := c.Enum
				cc.Enum = &e
			}
			ct.Columns = append(ct.Columns, cc)
		}
		fk := func(f services.ForeignKeyDoc) gen.CatalogForeignKey {
			return gen.CatalogForeignKey{Name: f.Name, Columns: f.Columns, Table: f.Table, RefColumns: f.RefCols, Embed: f.Embed, Multiple: f.Multiple}
		}
		for _, f := range t.ForeignKeys {
			ct.ForeignKeys = append(ct.ForeignKeys, fk(f))
		}
		for _, f := range t.ReferencedBy {
			ct.ReferencedBy = append(ct.ReferencedBy, fk(f))
		}
		for _, pol := range t.Policies {
			cp := gen.CatalogPolicy{Name: pol.Name, Command: pol.Command, Permissive: pol.Permissive, Roles: pol.Roles}
			if pol.Using != "" {
				u := pol.Using
				cp.Using = &u
			}
			if pol.Check != "" {
				c := pol.Check
				cp.Check = &c
			}
			ct.Policies = append(ct.Policies, cp)
		}
		for role, a := range t.Access {
			ct.Access[role] = gen.CatalogAccess{Select: a.Select, Insert: a.Insert, Update: a.Update, Delete: a.Delete}
		}
		out.Tables = append(out.Tables, ct)
	}
	for _, f := range d.Functions {
		cf := gen.CatalogFunction{Schema: f.Schema, Name: f.Name, Returns: f.Returns, ReturnsSet: f.ReturnsSet, Volatility: f.Volatility,
			SecurityDefiner: f.Definer}
		for _, a := range f.Args {
			cf.Args = append(cf.Args, struct {
				Name     string `json:"name"`
				Optional bool   `json:"optional"`
				Type     string `json:"type"`
			}{Name: a.Name, Optional: a.Optional, Type: a.Type})
		}
		if cf.Args == nil {
			cf.Args = []struct {
				Name     string `json:"name"`
				Optional bool   `json:"optional"`
				Type     string `json:"type"`
			}{}
		}
		out.Functions = append(out.Functions, cf)
	}
	writeJSON(w, http.StatusOK, out)
}
