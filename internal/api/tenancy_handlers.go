package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// requireTenancy answers 503 when the server runs without the tenancy
// service (no provisioning).
func (s *Server) requireTenancy(w http.ResponseWriter) bool {
	if s.tenancy == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "tenancy controls are not available on this server")
		return false
	}
	return true
}

// tenancyError maps the tenancy service's errors.
func (s *Server) tenancyError(w http.ResponseWriter, what string, err error) {
	var qe *tenancy.QuotaError
	switch {
	case errors.As(err, &qe):
		writeQuotaError(w, qe)
	case errors.Is(err, tenancy.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, tenancy.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, tenancy.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		s.provisionError(w, what, err)
	}
}

func writeQuotaError(w http.ResponseWriter, qe *tenancy.QuotaError) {
	limit := qe.Max
	writeJSON(w, http.StatusConflict, gen.Error{
		Code: "quota_exceeded", Message: fmt.Sprintf("your plan allows %d (%s); you are using %d", qe.Max, qe.Limit, qe.Used),
		Quota: &gen.QuotaItem{Limit: qe.Limit, Used: float32(qe.Used), Max: &limit},
	})
}

// checkQuota runs a quota check, answering the refusal; true means go on.
func (s *Server) checkQuota(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	s.tenancyError(w, "quota", err)
	return false
}

func genAllowance(a store.DedicatedAllowance, unlimited bool) gen.DedicatedAllowance {
	return gen.DedicatedAllowance{Instances: a.Instances, Cpus: float32(a.CPUs), MemoryMb: a.MemoryMB, DiskGb: a.DiskGB, Unlimited: &unlimited}
}

// GetOrgQuotas implements GET /api/v1/orgs/{org}/quotas.
func (s *Server) GetOrgQuotas(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	if !s.requireTenancy(w) {
		return
	}
	qs, o, err := s.tenancy.Quotas(r.Context(), org)
	if err != nil {
		s.internalError(w, "quotas", err)
		return
	}
	allow, err := store.DecodeDedicatedAllowance(o.DedicatedAllowance)
	if err != nil {
		s.internalError(w, "quotas", err)
		return
	}
	use, err := store.New(s.db).OrgDedicatedUse(r.Context(), org)
	if err != nil {
		s.internalError(w, "quotas", err)
		return
	}
	out := gen.OrgQuotas{
		Plan:               o.PlanName,
		Items:              make([]gen.QuotaItem, 0, len(qs)),
		DedicatedAllowance: genAllowance(allow, o.PlanName == tenancy.UnlimitedPlan),
		DedicatedUse:       gen.DedicatedAllowance{Instances: int(use.Instances), Cpus: float32(use.Cpus), MemoryMb: int(use.MemMb), DiskGb: int(use.DiskGb)},
	}
	for _, q := range qs {
		out.Items = append(out.Items, gen.QuotaItem{Limit: q.Limit, Used: float32(q.Used), Max: q.Max})
	}
	writeJSON(w, http.StatusOK, out)
}

func usageRange(from, to *time.Time, now time.Time) (time.Time, time.Time) {
	end := now
	if to != nil {
		end = *to
	}
	start := time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC)
	if from != nil {
		start = *from
	}
	return start, end
}

// GetOrgUsage implements GET /api/v1/orgs/{org}/usage.
func (s *Server) GetOrgUsage(w http.ResponseWriter, r *http.Request, org gen.OrgID, p gen.GetOrgUsageParams) {
	from, to := usageRange(p.From, p.To, time.Now().UTC())
	if !to.After(from) || to.Sub(from) > 400*24*time.Hour {
		writeError(w, http.StatusBadRequest, "bad_request", "the range must be positive and at most 400 days")
		return
	}
	q := store.New(s.db)
	rows, err := q.OrgUsage(r.Context(), store.OrgUsageParams{OrgID: org, FromTs: from, ToTs: to, Metric: p.Metric})
	if err != nil {
		s.internalError(w, "usage", err)
		return
	}
	names := map[uuid.UUID]string{}
	if ps, err := q.ListOrgProjectNames(r.Context(), org); err == nil {
		for _, pn := range ps {
			names[pn.ID] = pn.Name
		}
	}
	out := gen.UsageReport{From: from, To: to, Records: make([]gen.UsageRecord, 0, len(rows))}
	for _, m := range tenancy.UsageMetrics {
		out.Metrics = append(out.Metrics, gen.UsageMetric{Name: m.Name, Unit: m.Unit, Granularity: gen.UsageMetricGranularity(m.Granularity)})
	}
	totals := map[string]float64{}
	var order []string
	for _, u := range rows {
		rec := gen.UsageRecord{Metric: u.Metric, Granularity: gen.UsageRecordGranularity(u.Granularity), PeriodStart: u.PeriodStart, Quantity: float32(u.Quantity)}
		if u.ProjectID != uuid.Nil {
			id := u.ProjectID
			rec.ProjectId = &id
			if n, ok := names[id]; ok {
				rec.ProjectName = &n
			}
		}
		out.Records = append(out.Records, rec)
		if _, ok := totals[u.Metric]; !ok {
			order = append(order, u.Metric)
		}
		totals[u.Metric] += u.Quantity
	}
	for _, m := range order {
		out.Totals = append(out.Totals, gen.UsageTotal{Metric: m, Quantity: float32(totals[m])})
	}
	if out.Totals == nil {
		out.Totals = []gen.UsageTotal{}
	}
	if (p.Format != nil && *p.Format == gen.GetOrgUsageParamsFormatCsv) || strings.Contains(r.Header.Get("Accept"), "text/csv") {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="xenibase-usage.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"period_start", "granularity", "metric", "project_id", "project", "quantity"})
		for _, u := range rows {
			pid, name := "", ""
			if u.ProjectID != uuid.Nil {
				pid, name = u.ProjectID.String(), names[u.ProjectID]
			}
			_ = cw.Write([]string{u.PeriodStart.UTC().Format(time.RFC3339), u.Granularity, u.Metric, pid, name,
				strconv.FormatFloat(u.Quantity, 'f', -1, 64)})
		}
		cw.Flush()
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// DeleteOrg implements DELETE /api/v1/orgs/{org}.
func (s *Server) DeleteOrg(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	if !s.requireTenancy(w) {
		return
	}
	var req gen.DeleteOrgRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	auditFrom(r.Context()).target("org", org.String())
	sess, _ := sessionFrom(r.Context())
	after, err := s.tenancy.RequestOrgDeletion(r.Context(), org, req.Confirm, req.DeleteProjects != nil && *req.DeleteProjects, sess.UserID)
	if err != nil {
		s.tenancyError(w, "delete org", err)
		return
	}
	writeJSON(w, http.StatusAccepted, gen.OrgDeletion{DeleteAfter: after})
}

// CancelOrgDeletion implements POST /api/v1/orgs/{org}/cancel-deletion.
func (s *Server) CancelOrgDeletion(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	if !s.requireTenancy(w) {
		return
	}
	auditFrom(r.Context()).target("org", org.String())
	o, err := store.New(s.db).GetOrg(r.Context(), org)
	if err != nil {
		s.tenancyError(w, "cancel deletion", err)
		return
	}
	if o.Status != tenancy.OrgDeleting {
		writeError(w, http.StatusConflict, "conflict", "the organisation is not being deleted")
		return
	}
	if err := s.tenancy.Reinstate(r.Context(), org); err != nil {
		s.tenancyError(w, "cancel deletion", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// EndBreakGlass implements POST /api/v1/orgs/{org}/break-glass/{session_id}/end.
func (s *Server) EndBreakGlass(w http.ResponseWriter, r *http.Request, org gen.OrgID, id openapi_types.UUID) {
	if !s.requireTenancy(w) {
		return
	}
	auditFrom(r.Context()).target("break_glass", id.String())
	sess, _ := sessionFrom(r.Context())
	if err := s.tenancy.EndBreakGlass(r.Context(), org, id, &sess.UserID); err != nil {
		s.tenancyError(w, "end break-glass", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func genDedicatedRequest(id, org, project uuid.UUID, orgName *string, projectName, requester string, profile json.RawMessage,
	reason *string, status string, note *string, decided *time.Time, created time.Time) gen.DedicatedRequest {
	var pr struct {
		Profile  string     `json:"profile"`
		VolumeGB int        `json:"volume_gb"`
		NodeID   *uuid.UUID `json:"node_id"`
	}
	_ = json.Unmarshal(profile, &pr)
	return gen.DedicatedRequest{
		Id: id, OrgId: org, OrgName: orgName, ProjectId: project, ProjectName: projectName, RequestedBy: requester,
		Profile: pr.Profile, VolumeGb: pr.VolumeGB, NodeId: pr.NodeID, Reason: reason,
		Status: gen.DedicatedRequestStatus(status), DecisionNote: note, DecidedAt: decided, CreatedAt: created,
	}
}

// ListOrgDedicatedRequests implements GET /api/v1/orgs/{org}/dedicated-requests.
func (s *Server) ListOrgDedicatedRequests(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	rows, err := store.New(s.db).OrgDedicatedRequests(r.Context(), org)
	if err != nil {
		s.internalError(w, "dedicated requests", err)
		return
	}
	out := gen.DedicatedRequestList{Items: make([]gen.DedicatedRequest, 0, len(rows))}
	for _, d := range rows {
		out.Items = append(out.Items, genDedicatedRequest(d.ID, d.OrgID, d.ProjectID, nil, d.ProjectName, d.RequestedByEmail,
			d.Profile, d.Reason, d.Status, d.DecisionNote, d.DecidedAt, d.CreatedAt))
	}
	writeJSON(w, http.StatusOK, out)
}

// SwitchProjectCredentials implements POST /api/v1/projects/{id}/switch-credentials.
func (s *Server) SwitchProjectCredentials(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireProjects(w) {
		return
	}
	var req gen.SwitchCredentialsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	grace := provision.DefaultCredentialGrace
	if req.GraceDays != nil {
		grace = time.Duration(*req.GraceDays) * 24 * time.Hour
	}
	auditFrom(r.Context()).target("project", id.String())
	sw, err := s.projects.SwitchCredentials(r.Context(), id, grace, time.Now(), userID(r.Context()))
	if errors.Is(err, provision.ErrNotLegacy) {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		s.provisionError(w, "switch credentials", err)
		return
	}
	p := sw.Project
	p.OwnerRole = provision.OwnerRoleName(p.DbName)
	p.LegacyUntil = &sw.Until
	ap, err := s.toAPIProject(p)
	if err != nil {
		s.internalError(w, "switch credentials", err)
		return
	}
	op, err := toAPIOperation(sw.Operation)
	if err != nil {
		s.internalError(w, "switch credentials", err)
		return
	}
	writeJSON(w, http.StatusAccepted, gen.SwitchedCredentials{
		Project: ap, Operation: op, Password: sw.Password, LegacyUntil: sw.Until,
		Connection: toAPIConnection(s.projects.ConnectionFor(p), sw.Password),
	})
}

// GetProjectStorage implements GET /api/v1/projects/{id}/storage.
func (s *Server) GetProjectStorage(w http.ResponseWriter, r *http.Request, _ gen.ProjectID) {
	if !s.requireProjects(w) || !s.requireTenancy(w) {
		return
	}
	p, err := s.tenantProjectLive(r.Context())
	if err != nil {
		s.provisionError(w, "storage", err)
		return
	}
	out := gen.ProjectStorage{State: gen.StorageState(p.StorageState), Tables: []gen.TableFootprint{}}
	var size float64
	if err := s.db.QueryRow(r.Context(), `SELECT value FROM metric_points WHERE scope = 'project' AND scope_id = $1
		AND metric = 'size_bytes' AND resolution = '1m' ORDER BY ts DESC LIMIT 1`, p.ID).Scan(&size); err == nil {
		b := int64(size)
		out.SizeBytes = &b
	}
	if l, _, err := s.tenancy.Limits(r.Context(), p.OrgID); err == nil && p.Tier == provision.TierShared {
		if mb, ok := l.Get(store.LimitProjectStorageMB); ok {
			b := mb << 20
			out.LimitBytes = &b
		}
	}
	if p.Status == provision.StatusActive {
		ts, err := s.projects.LargestTables(r.Context(), p, 10)
		if err != nil {
			s.internalError(w, "storage", err)
			return
		}
		for _, t := range ts {
			out.Tables = append(out.Tables, gen.TableFootprint{Schema: t.Schema, Table: t.Table, Bytes: t.Bytes, DeadRows: t.DeadRows})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ReclaimSpace implements POST /api/v1/projects/{id}/reclaim-space.
func (s *Server) ReclaimSpace(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireProjects(w) {
		return
	}
	var req gen.ReclaimSpaceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("table", req.Schema+"."+req.Table)
	if s.tenancy != nil && !s.checkQuota(w, s.tenancy.CheckOperation(r.Context(), accessFrom(r.Context()).OrgID)) {
		return
	}
	op, err := s.projects.ReclaimSpace(r.Context(), id, req.Schema, req.Table, userID(r.Context()))
	if err != nil {
		s.provisionError(w, "reclaim space", err)
		return
	}
	s.writeOperation(w, "reclaim space", op)
}

// ListReapedSessions implements GET /api/v1/projects/{id}/reaped.
func (s *Server) ListReapedSessions(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	acc := accessFrom(r.Context())
	rows, err := store.New(s.db).ListReapedSessions(r.Context(), store.ListReapedSessionsParams{ProjectID: id, OrgID: acc.OrgID, MaxRows: 50})
	if err != nil {
		s.internalError(w, "reaped sessions", err)
		return
	}
	out := gen.ReapedSessionList{Items: make([]gen.ReapedSession, 0, len(rows))}
	for _, rs := range rows {
		out.Items = append(out.Items, gen.ReapedSession{Kind: gen.ReapedSessionKind(rs.Kind), Role: rs.RoleName,
			DurationS: int(rs.DurationS), Query: rs.Query, CreatedAt: rs.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- Platform admin (V2 §2.4, §12) --------------------------------------------

func genAdminOrgSummary(o store.AdminListOrgsRow) gen.AdminOrgSummary {
	orgTargets := int(o.OrgTargetProjects)
	return gen.AdminOrgSummary{
		OrgTargetProjects: &orgTargets,
		Id:                o.ID, Name: o.Name, Slug: o.Slug, Plan: o.PlanName, Status: gen.AdminOrgSummaryStatus(o.Status),
		SuspendedReason: o.SuspendedReason, Personal: o.PersonalOwnerID != nil, MemberCount: int(o.MemberCount),
		ProjectCount: int(o.ProjectCount), SizeBytes: int64(o.SizeBytes), OutboundDisabled: o.OutboundDisabled, CreatedAt: o.CreatedAt,
	}
}

// AdminListOrgs implements GET /api/v1/admin/orgs.
func (s *Server) AdminListOrgs(w http.ResponseWriter, r *http.Request, p gen.AdminListOrgsParams) {
	var search *string
	if p.Q != nil && strings.TrimSpace(*p.Q) != "" {
		v := strings.TrimSpace(*p.Q)
		search = &v
	}
	rows, err := store.New(s.db).AdminListOrgs(r.Context(), search)
	if err != nil {
		s.internalError(w, "admin orgs", err)
		return
	}
	out := gen.AdminOrgList{Items: make([]gen.AdminOrgSummary, 0, len(rows))}
	for _, o := range rows {
		out.Items = append(out.Items, genAdminOrgSummary(o))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminOrg(w http.ResponseWriter, r *http.Request, org uuid.UUID) {
	if !s.requireTenancy(w) {
		return
	}
	q := store.New(s.db)
	limits, o, err := s.tenancy.Limits(r.Context(), org)
	if err != nil {
		s.tenancyError(w, "admin org", err)
		return
	}
	var summary *gen.AdminOrgSummary
	rows, err := q.AdminListOrgs(r.Context(), nil)
	if err != nil {
		s.internalError(w, "admin org", err)
		return
	}
	for _, row := range rows {
		if row.ID == org {
			g := genAdminOrgSummary(row)
			summary = &g
		}
	}
	if summary == nil {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	overrides := map[string]*int64{}
	_ = json.Unmarshal(o.LimitOverrides, &overrides)
	allow, _ := store.DecodeDedicatedAllowance(o.DedicatedAllowance)
	out := gen.AdminOrg{
		Org: *summary, PlanId: o.PlanID, Limits: map[string]int64(limits), LimitOverrides: overrides,
		DedicatedAllowance: genAllowance(allow, o.PlanName == tenancy.UnlimitedPlan),
		Clusters:           []gen.SharedCluster{}, BreakGlass: []gen.BreakGlassSession{},
	}
	if cs, err := q.OrgSharedInstances(r.Context(), &org); err == nil {
		for _, c := range cs {
			n, _ := q.CountInstanceProjects(r.Context(), c.ID)
			out.Clusters = append(out.Clusters, gen.SharedCluster{Id: c.ID, NodeName: c.NodeName, OrgId: c.OrgID, OrgName: &summary.Name, ProjectCount: int(n)})
		}
	}
	sess, _ := sessionFrom(r.Context())
	if bg, err := q.ActiveBreakGlass(r.Context(), store.ActiveBreakGlassParams{OrgID: org, AdminID: sess.UserID}); err == nil {
		out.BreakGlass = append(out.BreakGlass, gen.BreakGlassSession{Id: bg.ID, OrgId: bg.OrgID, AdminEmail: sess.Email,
			Reason: bg.Reason, StartsAt: bg.StartsAt, ExpiresAt: bg.ExpiresAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// AdminGetOrg implements GET /api/v1/admin/orgs/{org}.
func (s *Server) AdminGetOrg(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	s.adminOrg(w, r, org)
}

// AdminUpdateOrg implements PATCH /api/v1/admin/orgs/{org}.
func (s *Server) AdminUpdateOrg(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	var req gen.AdminUpdateOrgRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("org", org.String())
	q := store.New(s.db)
	o, err := q.GetOrg(r.Context(), org)
	if err != nil {
		s.tenancyError(w, "admin update org", err)
		return
	}
	plan, overrides, allowance, outbound := o.PlanID, o.LimitOverrides, o.DedicatedAllowance, o.OutboundDisabled
	if req.PlanId != nil {
		if _, err := q.GetPlan(r.Context(), *req.PlanId); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "no such plan")
			return
		}
		plan = *req.PlanId
		a.set("plan_id", plan.String())
	}
	if req.LimitOverrides != nil {
		raw, _ := json.Marshal(req.LimitOverrides)
		if err := store.ValidateLimits(raw, true); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		overrides = raw
		a.set("limit_overrides", req.LimitOverrides)
	}
	if req.DedicatedAllowance != nil {
		d := req.DedicatedAllowance
		if d.Instances < 0 || d.Cpus < 0 || d.MemoryMb < 0 || d.DiskGb < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "the dedicated allowance must not be negative")
			return
		}
		allowance, _ = json.Marshal(store.DedicatedAllowance{Instances: d.Instances, CPUs: float64(d.Cpus), MemoryMB: d.MemoryMb, DiskGB: d.DiskGb})
		a.set("dedicated_allowance", req.DedicatedAllowance)
	}
	if req.OutboundDisabled != nil {
		outbound = *req.OutboundDisabled
		a.set("outbound_disabled", outbound)
	}
	if err := q.AdminUpdateOrg(r.Context(), store.AdminUpdateOrgParams{OrgID: org, PlanID: plan, LimitOverrides: overrides,
		DedicatedAllowance: allowance, OutboundDisabled: outbound}); err != nil {
		s.internalError(w, "admin update org", err)
		return
	}
	s.adminOrg(w, r, org)
}

// AdminSuspendOrg implements POST /api/v1/admin/orgs/{org}/suspend.
func (s *Server) AdminSuspendOrg(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	if !s.requireTenancy(w) {
		return
	}
	var req gen.ReasonRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("org", org.String())
	a.set("reason", req.Reason)
	if err := s.tenancy.Suspend(r.Context(), org, req.Reason); err != nil {
		s.tenancyError(w, "suspend org", err)
		return
	}
	s.orgEvent(r, org, "org.suspended", map[string]any{"reason": req.Reason})
	w.WriteHeader(http.StatusNoContent)
}

// AdminReinstateOrg implements POST /api/v1/admin/orgs/{org}/reinstate.
func (s *Server) AdminReinstateOrg(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	if !s.requireTenancy(w) {
		return
	}
	auditFrom(r.Context()).target("org", org.String())
	if err := s.tenancy.Reinstate(r.Context(), org); err != nil {
		s.tenancyError(w, "reinstate org", err)
		return
	}
	s.orgEvent(r, org, "org.reinstated", nil)
	w.WriteHeader(http.StatusNoContent)
}

// orgEvent writes a platform admin's action on an organisation to the
// org's own audit log too (V2 §10.8: "recorded in both audit logs").
func (s *Server) orgEvent(r *http.Request, org uuid.UUID, action string, detail map[string]any) {
	sess, _ := sessionFrom(r.Context())
	b, _ := json.Marshal(detail)
	target := "org"
	id := org.String()
	s.audit.Write(r.Context(), store.InsertAuditParams{
		UserID: &sess.UserID, Action: action, Detail: b, Ip: ipFrom(r.Context()), Outcome: "success",
		ActorKind: "session", OrgID: &org, TargetType: &target, TargetID: &id,
	})
}

// AdminSetOrgCluster implements POST /api/v1/admin/orgs/{org}/cluster.
func (s *Server) AdminSetOrgCluster(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	var req gen.SetOrgClusterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("org", org.String())
	a.set("instance_id", req.InstanceId.String())
	q := store.New(s.db)
	reserve := req.Reserved == nil || *req.Reserved
	var tag *uuid.UUID
	if reserve {
		n, err := q.SharedInstanceForeignProjects(r.Context(), store.SharedInstanceForeignProjectsParams{InstanceID: req.InstanceId, OrgID: org})
		if err != nil {
			s.internalError(w, "set org cluster", err)
			return
		}
		if n > 0 {
			writeError(w, http.StatusConflict, "conflict", fmt.Sprintf("%d project(s) of other organisations are on that cluster; use an empty one", n))
			return
		}
		tag = &org
	}
	n, err := q.SetInstanceOrg(r.Context(), store.SetInstanceOrgParams{ID: req.InstanceId, OrgID: tag})
	if err != nil {
		s.internalError(w, "set org cluster", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no such shared cluster")
		return
	}
	s.adminOrg(w, r, org)
}

// AdminStartBreakGlass implements POST /api/v1/admin/orgs/{org}/break-glass.
func (s *Server) AdminStartBreakGlass(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	if !s.requireTenancy(w) {
		return
	}
	var req gen.BreakGlassRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("org", org.String())
	a.set("reason", req.Reason)
	a.set("duration_minutes", req.DurationMinutes)
	sess, _ := sessionFrom(r.Context())
	bg, err := s.tenancy.StartBreakGlass(r.Context(), org, sess.UserID, sess.Email, req.Reason, time.Duration(req.DurationMinutes)*time.Minute)
	if err != nil {
		s.tenancyError(w, "break-glass", err)
		return
	}
	s.orgEvent(r, org, "org.break_glass.start", map[string]any{"reason": req.Reason, "expires_at": bg.ExpiresAt})
	writeJSON(w, http.StatusCreated, gen.BreakGlassSession{Id: bg.ID, OrgId: bg.OrgID, AdminEmail: sess.Email,
		Reason: bg.Reason, StartsAt: bg.StartsAt, ExpiresAt: bg.ExpiresAt})
}

func (s *Server) genPlan(r *http.Request, p store.QuotaPlan) (gen.Plan, error) {
	l, err := store.DecodePlanLimits(p.Limits)
	if err != nil {
		return gen.Plan{}, err
	}
	var n int
	_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM organizations WHERE plan_id = $1 AND status <> 'deleted'`, p.ID).Scan(&n)
	return gen.Plan{Id: p.ID, Name: p.Name, Limits: map[string]int64(l), OrgCount: n}, nil
}

// ListPlans implements GET /api/v1/admin/plans.
func (s *Server) ListPlans(w http.ResponseWriter, r *http.Request) {
	rows, err := store.New(s.db).ListPlans(r.Context())
	if err != nil {
		s.internalError(w, "plans", err)
		return
	}
	out := gen.PlanList{Items: make([]gen.Plan, 0, len(rows)), Keys: store.LimitKeys}
	for _, p := range rows {
		g, err := s.genPlan(r, p)
		if err != nil {
			s.internalError(w, "plans", err)
			return
		}
		out.Items = append(out.Items, g)
	}
	writeJSON(w, http.StatusOK, out)
}

func planInput(w http.ResponseWriter, req gen.PlanRequest) (string, json.RawMessage, bool) {
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 64 {
		writeError(w, http.StatusBadRequest, "bad_request", "a plan needs a name (up to 64 characters)")
		return "", nil, false
	}
	raw, _ := json.Marshal(req.Limits)
	if err := store.ValidateLimits(raw, false); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return "", nil, false
	}
	return name, raw, true
}

// CreatePlan implements POST /api/v1/admin/plans.
func (s *Server) CreatePlan(w http.ResponseWriter, r *http.Request) {
	var req gen.PlanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name, limits, ok := planInput(w, req)
	if !ok {
		return
	}
	p, err := store.New(s.db).InsertPlan(r.Context(), store.InsertPlanParams{Name: name, Limits: limits})
	if isUniqueViolation(err) {
		writeError(w, http.StatusConflict, "conflict", "a plan with that name exists")
		return
	}
	if err != nil {
		s.internalError(w, "create plan", err)
		return
	}
	auditFrom(r.Context()).target("plan", p.ID.String())
	g, err := s.genPlan(r, p)
	if err != nil {
		s.internalError(w, "create plan", err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

// UpdatePlan implements PATCH /api/v1/admin/plans/{plan_id}.
func (s *Server) UpdatePlan(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	var req gen.PlanRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name, limits, ok := planInput(w, req)
	if !ok {
		return
	}
	auditFrom(r.Context()).target("plan", id.String())
	p, err := store.New(s.db).UpdatePlan(r.Context(), store.UpdatePlanParams{ID: id, Name: name, Limits: limits})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "no such plan")
		return
	}
	if isUniqueViolation(err) {
		writeError(w, http.StatusConflict, "conflict", "a plan with that name exists")
		return
	}
	if err != nil {
		s.internalError(w, "update plan", err)
		return
	}
	g, err := s.genPlan(r, p)
	if err != nil {
		s.internalError(w, "update plan", err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// ListDedicatedRequests implements GET /api/v1/admin/dedicated-requests.
func (s *Server) ListDedicatedRequests(w http.ResponseWriter, r *http.Request, p gen.ListDedicatedRequestsParams) {
	var status *string
	if p.Status != nil {
		v := string(*p.Status)
		status = &v
	}
	rows, err := store.New(s.db).ListDedicatedRequests(r.Context(), status)
	if err != nil {
		s.internalError(w, "dedicated requests", err)
		return
	}
	out := gen.DedicatedRequestList{Items: make([]gen.DedicatedRequest, 0, len(rows))}
	for _, d := range rows {
		name := d.OrgName
		out.Items = append(out.Items, genDedicatedRequest(d.ID, d.OrgID, d.ProjectID, &name, d.ProjectName, d.RequestedByEmail,
			d.Profile, d.Reason, d.Status, d.DecisionNote, d.DecidedAt, d.CreatedAt))
	}
	writeJSON(w, http.StatusOK, out)
}

type requestProfile struct {
	Profile  string     `json:"profile"`
	VolumeGB int        `json:"volume_gb"`
	NodeID   *uuid.UUID `json:"node_id,omitempty"`
}

// ApproveDedicatedRequest implements POST /api/v1/admin/dedicated-requests/{request_id}/approve:
// the promotion starts, and the request is marked approved once it is queued.
func (s *Server) ApproveDedicatedRequest(w http.ResponseWriter, r *http.Request, id gen.RequestID) {
	ds := s.dedicatedSvc(w)
	if ds == nil {
		return
	}
	var req gen.DecideRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	q := store.New(s.db)
	dr, err := q.GetDedicatedRequest(r.Context(), id)
	if err != nil {
		s.tenancyError(w, "approve request", err)
		return
	}
	a := auditFrom(r.Context())
	a.target("dedicated_request", id.String())
	if dr.Status != "pending" {
		writeError(w, http.StatusConflict, "conflict", "the request is "+dr.Status)
		return
	}
	var prof requestProfile
	_ = json.Unmarshal(dr.Profile, &prof)
	sess, _ := sessionFrom(r.Context())
	op, err := ds.Promote(r.Context(), dedicated.PromoteParams{ProjectID: dr.ProjectID, NodeID: prof.NodeID, Profile: prof.Profile,
		VolumeGB: prof.VolumeGB, CreatedBy: &dr.RequestedBy})
	if err != nil {
		s.provisionError(w, "approve request", err)
		return
	}
	if _, err := q.DecideDedicatedRequest(r.Context(), store.DecideDedicatedRequestParams{ID: id, Status: "approved", DecidedBy: &sess.UserID, Note: req.Note}); err != nil {
		s.internalError(w, "approve request", err)
		return
	}
	if req.RaiseAllowance != nil && *req.RaiseAllowance {
		if err := s.raiseAllowance(r, dr.OrgID, prof); err != nil {
			s.log.Warn("raise dedicated allowance", "org_id", dr.OrgID, "err", err)
		}
	}
	s.mailRequester(r, dr, "approved", req.Note)
	s.orgEvent(r, dr.OrgID, "org.dedicated_request.approved", map[string]any{"request_id": id.String(), "project_id": dr.ProjectID.String()})
	s.writeOperation(w, "approve request", op)
}

// raiseAllowance grows an organisation's allowance by one instance of prof.
func (s *Server) raiseAllowance(r *http.Request, org uuid.UUID, prof requestProfile) error {
	q := store.New(s.db)
	o, err := q.GetOrg(r.Context(), org)
	if err != nil {
		return err
	}
	a, err := store.DecodeDedicatedAllowance(o.DedicatedAllowance)
	if err != nil {
		return err
	}
	pr, ok := dedicated.ProfileByName(prof.Profile)
	if !ok {
		pr, _ = dedicated.ProfileByName(dedicated.DefaultProfile)
	}
	vol := prof.VolumeGB
	if vol == 0 {
		vol = dedicated.DefaultVolumeGB
	}
	a.Instances++
	a.CPUs += pr.CPUs
	a.MemoryMB += pr.MemoryMB
	a.DiskGB += vol
	raw, _ := json.Marshal(a)
	return q.SetOrgDedicatedAllowance(r.Context(), store.SetOrgDedicatedAllowanceParams{OrgID: org, Allowance: raw})
}

// RejectDedicatedRequest implements POST /api/v1/admin/dedicated-requests/{request_id}/reject.
func (s *Server) RejectDedicatedRequest(w http.ResponseWriter, r *http.Request, id gen.RequestID) {
	var req gen.DecideRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	q := store.New(s.db)
	dr, err := q.GetDedicatedRequest(r.Context(), id)
	if err != nil {
		s.tenancyError(w, "reject request", err)
		return
	}
	auditFrom(r.Context()).target("dedicated_request", id.String())
	sess, _ := sessionFrom(r.Context())
	n, err := q.DecideDedicatedRequest(r.Context(), store.DecideDedicatedRequestParams{ID: id, Status: "rejected", DecidedBy: &sess.UserID, Note: req.Note})
	if err != nil {
		s.internalError(w, "reject request", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusConflict, "conflict", "the request is "+dr.Status)
		return
	}
	s.mailRequester(r, dr, "rejected", req.Note)
	s.orgEvent(r, dr.OrgID, "org.dedicated_request.rejected", map[string]any{"request_id": id.String()})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) mailRequester(r *http.Request, dr store.DedicatedRequest, outcome string, note *string) {
	if s.mail == nil {
		return
	}
	var email, project string
	if err := s.db.QueryRow(r.Context(), `SELECT u.email, p.name FROM users u, projects p WHERE u.id = $1 AND p.id = $2`,
		dr.RequestedBy, dr.ProjectID).Scan(&email, &project); err != nil {
		return
	}
	body := fmt.Sprintf("Your request to move %s to a dedicated instance was %s.", project, outcome)
	if outcome == "approved" {
		body += " The promotion has started."
	}
	if note != nil && *note != "" {
		body += "\n\nNote from the platform admin: " + *note
	}
	if err := s.mail.Send(r.Context(), mailMessage([]string{email}, "[Xenibase] Dedicated instance request "+outcome, body)); err != nil {
		s.log.Warn("dedicated request email", "err", err)
	}
}

// PlatformUsage implements GET /api/v1/admin/usage.
func (s *Server) PlatformUsage(w http.ResponseWriter, r *http.Request, p gen.PlatformUsageParams) {
	from, to := usageRange(p.From, p.To, time.Now().UTC())
	rows, err := store.New(s.db).PlatformUsage(r.Context(), store.PlatformUsageParams{FromTs: from, ToTs: to})
	if err != nil {
		s.internalError(w, "platform usage", err)
		return
	}
	out := gen.PlatformUsage{From: from, To: to, Items: make([]gen.PlatformUsageRow, 0, len(rows))}
	for _, u := range rows {
		out.Items = append(out.Items, gen.PlatformUsageRow{OrgId: u.OrgID, OrgName: u.OrgName, Metric: u.Metric, Quantity: float32(u.Quantity)})
	}
	writeJSON(w, http.StatusOK, out)
}

// ListSharedClusters implements GET /api/v1/admin/shared-clusters.
func (s *Server) ListSharedClusters(w http.ResponseWriter, r *http.Request) {
	rows, err := store.New(s.db).ListSharedClusters(r.Context())
	if err != nil {
		s.internalError(w, "shared clusters", err)
		return
	}
	out := gen.SharedClusterList{Items: make([]gen.SharedCluster, 0, len(rows))}
	for _, c := range rows {
		out.Items = append(out.Items, gen.SharedCluster{Id: c.ID, NodeName: c.NodeName, OrgId: c.OrgID, OrgName: c.OrgName, ProjectCount: int(c.ProjectCount)})
	}
	writeJSON(w, http.StatusOK, out)
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func mailMessage(to []string, subject, body string) mail.Message {
	return mail.Message{To: to, Subject: subject, Body: body}
}

func ptrTo[T any](v T) *T { return &v }

// profileSize is the instance a dedicated profile and volume ask for.
func profileSize(profile string, volumeGB int) tenancy.Dedicated {
	pr, ok := dedicated.ProfileByName(profile)
	if !ok {
		pr, _ = dedicated.ProfileByName(dedicated.DefaultProfile)
	}
	if volumeGB == 0 {
		volumeGB = dedicated.DefaultVolumeGB
	}
	return tenancy.Dedicated{CPUs: pr.CPUs, MemoryMB: pr.MemoryMB, DiskGB: volumeGB}
}

func instanceSize(i store.Instance) tenancy.Dedicated {
	d := tenancy.Dedicated{}
	if f, err := i.CpuLimit.Float64Value(); err == nil && f.Valid {
		d.CPUs = f.Float64
	}
	if i.MemLimitMb != nil {
		d.MemoryMB = int(*i.MemLimitMb)
	}
	if i.VolumeGb != nil {
		d.DiskGB = int(*i.VolumeGb)
	}
	return d
}

// withinAllowance refuses a new dedicated instance beyond the
// organisation's allowance (V2 §10.6); true means go on.
func (s *Server) withinAllowance(w http.ResponseWriter, r *http.Request, org uuid.UUID, d tenancy.Dedicated) bool {
	if !s.checkQuota(w, s.tenancy.CheckSpendCap(r.Context(), org)) || !s.checkQuota(w, s.tenancy.CheckBillingStanding(r.Context(), org)) {
		return false
	}
	ok, err := s.tenancy.WithinAllowance(r.Context(), org, d)
	if err != nil {
		s.internalError(w, "dedicated allowance", err)
		return false
	}
	if !ok {
		use, _ := store.New(s.db).OrgDedicatedUse(r.Context(), org)
		writeJSON(w, http.StatusConflict, gen.Error{
			Code:    "quota_exceeded",
			Message: "this is beyond your organisation's dedicated allowance: create the project on the shared tier and request a promotion, which the platform admin approves",
			Quota:   &gen.QuotaItem{Limit: "dedicated_allowance", Used: float32(use.Instances)},
		})
		return false
	}
	return true
}

// requestDedicated records a dedicated request for the platform admin and
// emails them (V2 §10.6).
func (s *Server) requestDedicated(w http.ResponseWriter, r *http.Request, org, project uuid.UUID, pp dedicated.PromoteParams, reason *string) {
	q := store.New(s.db)
	p, err := s.tenantProjectLive(r.Context())
	if err != nil {
		s.provisionError(w, "promote", err)
		return
	}
	if p.Tier != provision.TierShared {
		writeError(w, http.StatusConflict, "conflict", "the project is already dedicated")
		return
	}
	if _, ok := dedicated.ProfileByName(pp.Profile); pp.Profile != "" && !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown profile "+pp.Profile)
		return
	}
	prof := requestProfile{Profile: pp.Profile, VolumeGB: pp.VolumeGB, NodeID: pp.NodeID}
	if prof.Profile == "" {
		prof.Profile = dedicated.DefaultProfile
	}
	if prof.VolumeGB == 0 {
		prof.VolumeGB = dedicated.DefaultVolumeGB
	}
	raw, _ := json.Marshal(prof)
	sess, _ := sessionFrom(r.Context())
	dr, err := q.InsertDedicatedRequest(r.Context(), store.InsertDedicatedRequestParams{
		OrgID: org, ProjectID: project, RequestedBy: sess.UserID, Profile: raw, Reason: reason,
	})
	if isUniqueViolation(err) {
		writeError(w, http.StatusConflict, "conflict", "this project already has a pending request")
		return
	}
	if err != nil {
		s.internalError(w, "dedicated request", err)
		return
	}
	a := auditFrom(r.Context())
	a.set("dedicated_request", dr.ID.String())
	if s.mail != nil {
		if admins, err := q.PlatformAdminEmails(r.Context()); err == nil && len(admins) > 0 {
			body := fmt.Sprintf("%s asks to move a project to a dedicated instance (%s, %d GB).", sess.Email, prof.Profile, prof.VolumeGB)
			if reason != nil && *reason != "" {
				body += "\n\nReason: " + *reason
			}
			body += "\n\nReview it in Admin -> Dedicated requests."
			if err := s.mail.Send(r.Context(), mailMessage(admins, "[Xenibase] Dedicated instance request", body)); err != nil {
				s.log.Warn("dedicated request email", "err", err)
			}
		}
	}
	writeJSON(w, http.StatusCreated, genDedicatedRequest(dr.ID, dr.OrgID, dr.ProjectID, nil, p.Name, sess.Email, dr.Profile,
		dr.Reason, dr.Status, dr.DecisionNote, dr.DecidedAt, dr.CreatedAt))
}
