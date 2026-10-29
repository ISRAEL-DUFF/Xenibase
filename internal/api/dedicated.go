package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/backup"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/faildomain"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
)

func (s *Server) dedicatedSvc(w http.ResponseWriter) *dedicated.Service {
	if s.backups == nil || s.backups.Dedicated == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "the dedicated tier is not available on this server")
		return nil
	}
	return s.backups.Dedicated
}

func i32(n *int32) *int {
	if n == nil {
		return nil
	}
	v := int(*n)
	return &v
}

// instanceSummaries maps live instances to their API summary.
func (s *Server) instanceSummaries(ctx context.Context) map[uuid.UUID]gen.InstanceSummary {
	out := map[uuid.UUID]gen.InstanceSummary{}
	if s.db == nil {
		return out
	}
	rows, err := store.New(s.db).ListInstanceSummaries(ctx)
	if err != nil {
		s.log.Warn("instance summaries", "err", err)
		return out
	}
	life := map[int]provision.PGVersion{}
	if s.projects != nil {
		if vs, err := s.projects.Versions(ctx); err == nil {
			for _, v := range vs {
				life[v.Major] = v
			}
		}
	}
	for _, r := range rows {
		sum := gen.InstanceSummary{
			Id: r.ID, Kind: gen.InstanceSummaryKind(r.Kind), Status: r.Status, Error: r.Error,
			NodeId: r.NodeID, NodeName: r.NodeName, Profile: r.Profile, MemoryMb: i32(r.MemLimitMb), VolumeGb: i32(r.VolumeGb),
			PgVersion: int(r.PgVersion), PgRelease: r.PgRelease, PgReleaseAvailable: r.PgReleaseAvailable, HaEnabled: &r.HaEnabled,
		}
		if r.Kind == "dedicated" {
			pd := int(r.PitrDays)
			mc, most := int(r.MaxConnections), provision.ConnectionLimitMax(r.MaxConnections)
			sum.PitrDays, sum.MaxConnections, sum.ConnectionLimitMax = &pd, &mc, &most
		}
		if v, ok := life[int(r.PgVersion)]; ok {
			st := gen.InstanceSummaryPgVersionStatus(v.Status)
			sum.PgVersionStatus, sum.PgVersionRetiresAt = &st, v.RetiresAt
		}
		if f, err := r.CpuLimit.Float64Value(); err == nil && f.Valid {
			v := float32(f.Float64)
			sum.Cpus = &v
		}
		out[r.ID] = sum
	}
	return out
}

// ListProfiles implements GET /api/v1/profiles.
func (s *Server) ListProfiles(w http.ResponseWriter, r *http.Request) {
	out := gen.ProfileList{DefaultProfile: dedicated.DefaultProfile, DefaultVolumeGb: dedicated.DefaultVolumeGB,
		PgVersions: provision.DefaultPGVersions, DefaultPgVersion: provision.DefaultPGVersions[len(provision.DefaultPGVersions)-1]}
	if s.projects != nil {
		// Open for new projects: installed, not retired, not in preview.
		vs, err := s.projects.Versions(r.Context())
		if err != nil {
			s.internalError(w, "profiles", err)
			return
		}
		out.PgVersions = []int{}
		var life []gen.PgVersionInfo
		for _, v := range vs {
			if !v.Installed {
				continue
			}
			life = append(life, toAPIPgVersion(v, false))
			if v.Status == provision.VersionSupported || v.Status == provision.VersionDeprecated {
				out.PgVersions = append(out.PgVersions, v.Major)
			}
		}
		out.PgVersionLifecycle = &life
		if d, err := s.projects.DefaultVersion(r.Context()); err == nil {
			out.DefaultPgVersion = d
		}
	}
	for _, p := range dedicated.Profiles {
		out.Items = append(out.Items, gen.Profile{Name: p.Name, Cpus: float32(p.CPUs), MemoryMb: p.MemoryMB})
	}
	writeJSON(w, http.StatusOK, out)
}

// RestoreProjectPITR implements POST /api/v1/projects/{id}/pitr.
func (s *Server) RestoreProjectPITR(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireBackups(w) || s.dedicatedSvc(w) == nil {
		return
	}
	var req gen.PitrRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	if req.TargetTime != nil {
		a.set("target_time", req.TargetTime.UTC())
	}
	if s.tenancy != nil {
		org := accessFrom(r.Context()).OrgID
		if !s.checkQuota(w, s.tenancy.CheckCreateProject(r.Context(), org)) || !s.checkQuota(w, s.tenancy.CheckOperation(r.Context(), org)) {
			return
		}
		// The recovery runs on a new instance the size of the source's.
		if src, err := s.tenantProjectLive(r.Context()); err == nil {
			if inst, err := store.New(s.db).GetInstance(r.Context(), src.InstanceID); err == nil && !s.withinAllowance(w, r, org, instanceSize(inst)) {
				return
			}
		}
	}
	c, err := s.backups.PITR(r.Context(), backup.PITRParams{ProjectID: id, TargetTime: req.TargetTime, Name: req.Name, CreatedBy: userID(r.Context()), CreatorRole: creatorRole(accessFrom(r.Context()))})
	if err != nil {
		s.backupError(w, "point-in-time recovery", err)
		return
	}
	a.set("new_project", c.Project.ID.String())
	s.writeCredentials(w, c.Project, c.Operation, c.Password)
}

// ProjectInstanceAction implements POST /api/v1/projects/{id}/instance.
func (s *Server) ProjectInstanceAction(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	var req gen.InstanceActionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("action", string(req.Action))
	p, err := s.tenantProjectLive(r.Context())
	if err != nil {
		s.provisionError(w, "instance action", err)
		return
	}
	res, err := ds.Act(r.Context(), p, string(req.Action))
	if err != nil {
		s.provisionError(w, "instance action", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.InstanceState{State: res.State, Running: res.Running, Container: &res.Container})
}

// CreateNode implements POST /api/v1/nodes.
func (s *Server) CreateNode(w http.ResponseWriter, r *http.Request) {
	if !s.requireBackups(w) {
		return
	}
	var req gen.CreateNodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.set("name", req.Name)
	a.set("role", string(req.Role))
	region, domain := "", ""
	if req.Region != nil {
		region = *req.Region
	}
	if req.FailureDomain != nil {
		domain = *req.FailureDomain
		a.set("failure_domain", domain)
	}
	n, tok, exp, err := s.nodes.CreateNode(r.Context(), req.Name, req.PrivateAddr, string(req.Role), region, domain)
	if err != nil {
		s.backupError(w, "create node", err)
		return
	}
	a.target("node", n.ID.String())
	out := gen.NodeCreated{
		Node: s.toAPINode(n), Token: tok, ExpiresAt: exp, Command: registerCommand(r, tok, req.PrivateAddr),
	}
	var warnings []string
	if n.Role == nodes.RolePooler {
		// The region's pooler hosts should be apart (V3.1 §2.2); Xenibase
		// can't move them, so it says so rather than refusing.
		if all, err := store.New(s.db).ListNodes(r.Context()); err == nil {
			for _, o := range all {
				if o.ID != n.ID && o.Role == nodes.RolePooler && o.Status != "removed" && o.Region == n.Region && !faildomain.Separated(o, n) {
					msg := fmt.Sprintf("%s is in the same failure domain as pooler host %s (%s): one failure could take both", n.Name, o.Name, faildomain.Label(o))
					warnings = append(warnings, msg)
				}
			}
		}
	}
	if len(warnings) > 0 {
		out.Warnings = &warnings
	}
	writeJSON(w, http.StatusCreated, out)
}

func registerCommand(r *http.Request, token, addr string) string {
	if addr == "" {
		addr = "<this node's private address>"
	}
	return fmt.Sprintf("xenibase-agent register --server https://%s --token %s --advertise %s:7070", r.Host, token, addr)
}

// GetNode implements GET /api/v1/nodes/{id}.
func (s *Server) GetNode(w http.ResponseWriter, r *http.Request, id gen.NodeID) {
	if !s.requireBackups(w) {
		return
	}
	q := store.New(s.db)
	n, err := q.GetNode(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "node not found")
		return
	}
	if err != nil {
		s.internalError(w, "get node", err)
		return
	}
	rows, err := q.ListNodeInstances(r.Context(), id)
	if err != nil {
		s.internalError(w, "get node", err)
		return
	}
	out := gen.NodeDetail{Node: s.toAPINode(n), Instances: []gen.NodeInstance{}}
	for _, i := range rows {
		ni := gen.NodeInstance{
			Id: i.ID, Kind: i.Kind, Status: i.Status, Error: i.Error, Profile: i.Profile,
			MemoryMb: i32(i.MemLimitMb), VolumeGb: i32(i.VolumeGb), Projects: int(i.Projects), CreatedAt: &i.CreatedAt,
			PgVersion: ptrTo(int(i.PgVersion)), PgRelease: i.PgRelease, PgReleaseAvailable: i.PgReleaseAvailable,
		}
		if f, err := i.CpuLimit.Float64Value(); err == nil && f.Valid {
			v := float32(f.Float64)
			ni.Cpus = &v
		}
		host := n.PrivateAddr
		if i.Host != nil {
			host = *i.Host
		}
		addr := fmt.Sprintf("%s:%d", host, i.Port)
		ni.Address = &addr
		out.Instances = append(out.Instances, ni)
	}
	writeJSON(w, http.StatusOK, out)
}

// RemoveNode implements DELETE /api/v1/nodes/{id}.
func (s *Server) RemoveNode(w http.ResponseWriter, r *http.Request, id gen.NodeID) {
	if !s.requireBackups(w) {
		return
	}
	auditFrom(r.Context()).target("node", id.String())
	if err := s.nodes.RemoveNode(r.Context(), id); err != nil {
		s.backupError(w, "remove node", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreateSharedCluster implements POST /api/v1/nodes/{id}/shared-cluster.
func (s *Server) CreateSharedCluster(w http.ResponseWriter, r *http.Request, id gen.NodeID) {
	ds := s.dedicatedSvc(w)
	if ds == nil {
		return
	}
	var req gen.SharedClusterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("node", id.String())
	a.set("memory_mb", req.MemoryMb)
	version := 0
	if req.PgVersion != nil {
		version = *req.PgVersion
		a.set("pg_version", version)
	}
	op, err := ds.AddSharedCluster(r.Context(), id, req.MemoryMb, version, userID(r.Context()))
	if err != nil {
		s.provisionError(w, "create shared cluster", err)
		return
	}
	s.writeOperation(w, "create shared cluster", op)
}

// UpdateNode implements PATCH /api/v1/nodes/{id}.
func (s *Server) UpdateNode(w http.ResponseWriter, r *http.Request, id gen.NodeID) {
	if !s.requireBackups(w) {
		return
	}
	var req gen.UpdateNodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("node", id.String())
	if req.Role == nil && req.FailureDomain == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "nothing to change: give role or failure_domain")
		return
	}
	var n store.Node
	var err error
	if req.Role != nil {
		a.set("role", string(*req.Role))
		if n, err = s.nodes.SetRole(r.Context(), id, string(*req.Role)); err != nil {
			s.backupError(w, "update node", err)
			return
		}
	}
	if req.FailureDomain != nil {
		a.set("failure_domain", *req.FailureDomain)
		if n, err = s.nodes.SetFailureDomain(r.Context(), id, *req.FailureDomain); err != nil {
			s.backupError(w, "update node", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.toAPINode(n))
}

// GetPromotionEstimate implements GET /api/v1/projects/{id}/promote.
func (s *Server) GetPromotionEstimate(w http.ResponseWriter, r *http.Request, _ gen.ProjectID) {
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	p, err := s.tenantProjectLive(r.Context())
	if err != nil {
		s.provisionError(w, "promotion estimate", err)
		return
	}
	if p.Tier != provision.TierShared {
		writeError(w, http.StatusBadRequest, "bad_request", "only shared projects can be promoted")
		return
	}
	est, err := ds.EstimatePromotion(r.Context(), p)
	if err != nil {
		s.internalError(w, "promotion estimate", err)
		return
	}
	out := gen.PromotionEstimate{SizeBytes: est.SizeBytes, EstimatedDowntimeSeconds: int(est.Downtime.Seconds())}
	if est.Mode != "" {
		m := gen.PromotionEstimateCopyMode(est.Mode)
		out.CopyMode = &m
	}
	if est.Reason != "" {
		out.FallbackReason = &est.Reason
	}
	writeJSON(w, http.StatusOK, out)
}

// PromoteProject implements POST /api/v1/projects/{id}/promote.
func (s *Server) PromoteProject(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	var req gen.PromoteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	pp := dedicated.PromoteParams{ProjectID: id, NodeID: req.NodeId, CreatedBy: userID(r.Context())}
	if req.Profile != nil {
		pp.Profile = *req.Profile
		a.set("profile", pp.Profile)
	}
	if req.VolumeGb != nil {
		pp.VolumeGB = *req.VolumeGb
	}
	if s.tenancy != nil {
		org := accessFrom(r.Context()).OrgID
		if !s.checkQuota(w, s.tenancy.CheckSpendCap(r.Context(), org)) {
			return
		}
		ok, err := s.tenancy.WithinAllowance(r.Context(), org, profileSize(pp.Profile, pp.VolumeGB))
		if err != nil {
			s.internalError(w, "promote", err)
			return
		}
		if !ok {
			// Beyond the allowance, the promotion becomes a request (V2 §10.6).
			s.requestDedicated(w, r, org, id, pp, req.Reason)
			return
		}
	}
	op, err := ds.Promote(r.Context(), pp)
	if err != nil {
		s.provisionError(w, "promote", err)
		return
	}
	s.writeOperation(w, "promote", op)
}

// demoteRequest reads an optional DemoteRequest body.
func demoteRequest(w http.ResponseWriter, r *http.Request) (gen.DemoteRequest, bool) {
	var req gen.DemoteRequest
	if !decodeOptionalJSON(w, r, &req) {
		return req, false
	}
	return req, true
}

func demoteOptions(req gen.DemoteRequest) dedicated.DemoteOptions {
	return dedicated.DemoteOptions{NodeID: req.NodeId, ConsoleWritable: req.ConsoleWritable != nil && *req.ConsoleWritable}
}

func toAPIDemotePlan(pl dedicated.DemotePlan) gen.DemotePreflight {
	out := gen.DemotePreflight{
		Eligible: len(pl.Blocked()) == 0, Checks: make([]gen.DemoteCheck, len(pl.Checks)),
		SizeBytes: pl.SizeBytes, EstimatedDowntimeSeconds: int(pl.Downtime.Seconds()), Resets: pl.Resets,
		RetainHours: int(pl.RetainFor.Hours()),
		SettingsAfter: gen.ProjectSettings{
			ConnectionLimit: pl.Settings.ConnectionLimit, PoolSize: pl.Settings.PoolSize,
			StatementTimeout: pl.Settings.StatementTimeout, IdleInTransactionSessionTimeout: pl.Settings.IdleInTransactionTimeout,
			DiskWarnBytes: pl.Settings.DiskWarnBytes, ConsoleReadOnly: pl.Settings.ConsoleReadOnly,
		},
	}
	if out.Resets == nil {
		out.Resets = []string{}
	}
	for i, c := range pl.Checks {
		out.Checks[i] = gen.DemoteCheck{Name: gen.DemoteCheckName(c.Name), Status: gen.DemoteCheckStatus(c.Status), Message: c.Message}
	}
	if t := pl.Target; t != nil {
		out.Target = &gen.DemoteTarget{NodeId: t.NodeID, NodeName: t.NodeName, OrgCluster: t.OrgCluster}
		if t.FreeBytes >= 0 {
			out.Target.FreeBytes = ptrTo(int64(t.FreeBytes))
		}
	}
	if pl.CopyMode != "" {
		m := gen.DemotePreflightCopyMode(pl.CopyMode)
		out.CopyMode = &m
	}
	if pl.CopyReason != "" {
		out.FallbackReason = &pl.CopyReason
	}
	return out
}

// DemotePreflight implements POST /api/v1/projects/{id}/demote/preflight.
func (s *Server) DemotePreflight(w http.ResponseWriter, r *http.Request, _ gen.ProjectID) {
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	req, ok := demoteRequest(w, r)
	if !ok {
		return
	}
	p, err := s.tenantProjectLive(r.Context())
	if err != nil {
		s.provisionError(w, "demotion preflight", err)
		return
	}
	plan, err := ds.DemotePreflight(r.Context(), p, demoteOptions(req))
	if err != nil {
		s.provisionError(w, "demotion preflight", err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIDemotePlan(plan))
}

// DemoteProject implements POST /api/v1/projects/{id}/demote.
func (s *Server) DemoteProject(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	req, ok := demoteRequest(w, r)
	if !ok {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	op, plan, err := ds.Demote(r.Context(), dedicated.DemoteParams{
		ProjectID: id, DemoteOptions: demoteOptions(req),
		AcceptWarnings: req.AcceptWarnings != nil && *req.AcceptWarnings, CreatedBy: userID(r.Context()),
	})
	if plan.Target != nil {
		a.set("node", plan.Target.NodeName)
	}
	if err != nil {
		s.provisionError(w, "demote", err)
		return
	}
	s.writeOperation(w, "demote", op)
}

// MoveProject implements POST /api/v1/admin/projects/{project_id}/move.
func (s *Server) MoveProject(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	var req gen.MoveProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("node_id", req.NodeId.String())
	op, err := ds.Move(r.Context(), dedicated.MoveParams{ProjectID: id, NodeID: req.NodeId, CreatedBy: userID(r.Context())})
	if err != nil {
		s.provisionError(w, "move", err)
		return
	}
	s.writeOperation(w, "move", op)
}

// CreateIntegratorRoles implements POST /api/v1/admin/projects/{project_id}/roles.
func (s *Server) CreateIntegratorRoles(w http.ResponseWriter, r *http.Request, id openapi_types.UUID) {
	var req gen.IntegratorRolesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := dedicated.ValidateIntegratorRoles(req.Roles); err != nil {
		s.provisionError(w, "integrator roles", err)
		return
	}
	p, err := store.New(s.db).GetProject(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && p.DeletedAt != nil) {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	if err != nil {
		s.internalError(w, "integrator roles", err)
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.orgID, a.projectID = p.OrgID, p.ID
	a.set("roles", req.Roles)
	if p.Tier != provision.TierDedicated {
		writeError(w, http.StatusConflict, "conflict", "dedicated projects only: "+p.Name+" is on the shared tier, whose roles are the platform's")
		return
	}
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	res, err := ds.CreateIntegratorRoles(r.Context(), p, req.Roles)
	if err != nil {
		s.provisionError(w, "integrator roles", err)
		return
	}
	a.set("created", res.Created)
	writeJSON(w, http.StatusOK, gen.IntegratorRoles{Roles: res.Roles, Created: res.Created})
}

// ListProjectMoves implements GET /api/v1/projects/{id}/moves.
func (s *Server) ListProjectMoves(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	list, err := store.New(s.db).LatestMoves(r.Context(), store.LatestMovesParams{ProjectID: id, Lim: 20})
	if err != nil {
		s.internalError(w, "list moves", err)
		return
	}
	out := gen.MoveList{Items: make([]gen.Move, 0, len(list))}
	for _, m := range list {
		mv := gen.Move{
			Id: m.ID, OperationId: m.OperationID, SourceInstance: m.SourceInstance, TargetInstance: m.TargetInstance,
			Mode: gen.MoveMode(m.Mode), FallbackReason: m.FallbackReason, Phase: gen.MovePhase(m.Phase),
			LagBytes: m.LagBytes, StartedAt: m.StartedAt, FinishedAt: m.FinishedAt,
		}
		if m.TablesTotal != nil {
			v := int(*m.TablesTotal)
			mv.TablesTotal = &v
		}
		if m.TablesReady != nil {
			v := int(*m.TablesReady)
			mv.TablesReady = &v
		}
		if m.FreezeMs != nil {
			v := int(*m.FreezeMs)
			mv.FreezeMs = &v
		}
		out.Items = append(out.Items, mv)
	}
	writeJSON(w, http.StatusOK, out)
}

func toAPIUpgradePlan(pl dedicated.UpgradePlan) gen.UpgradePreflight {
	out := gen.UpgradePreflight{
		Eligible: len(pl.Blocked()) == 0, From: pl.From, To: pl.To, Checks: make([]gen.UpgradeCheck, len(pl.Checks)),
		SizeBytes: pl.SizeBytes, EstimatedDowntimeSeconds: int(pl.Downtime.Seconds()),
		CopyMode: gen.UpgradePreflightCopyMode(pl.CopyMode),
	}
	if out.CopyMode == "" {
		out.CopyMode = gen.UpgradePreflightCopyModeLogical
	}
	for i, c := range pl.Checks {
		out.Checks[i] = gen.UpgradeCheck{Name: gen.UpgradeCheckName(c.Name), Status: gen.UpgradeCheckStatus(c.Status), Message: c.Message}
	}
	if pl.TargetNode != "" {
		out.TargetNode = &pl.TargetNode
	}
	if pl.CopyReason != "" {
		out.FallbackReason = &pl.CopyReason
	}
	return out
}

// UpgradePreflight implements POST /api/v1/projects/{id}/upgrade/preflight.
func (s *Server) UpgradePreflight(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	var req gen.UpgradeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := store.New(s.db).GetProject(r.Context(), id)
	if err != nil {
		s.provisionError(w, "upgrade preflight", err)
		return
	}
	pl, err := ds.UpgradePreflight(r.Context(), p, req.PgVersion)
	if err != nil {
		s.provisionError(w, "upgrade preflight", err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIUpgradePlan(pl))
}

// UpgradeProject implements POST /api/v1/projects/{id}/upgrade.
func (s *Server) UpgradeProject(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	var req gen.UpgradeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("pg_version", req.PgVersion)
	op, _, err := ds.Upgrade(r.Context(), dedicated.UpgradeParams{ProjectID: id, PgVersion: req.PgVersion, CreatedBy: userID(r.Context())})
	if err != nil {
		s.provisionError(w, "upgrade", err)
		return
	}
	s.writeOperation(w, "upgrade", op)
}

// manageRolesOut is a project's role management as the API shows it.
func (s *Server) manageRolesOut(r *http.Request, p store.Project, skipped map[string]string) (gen.ManageRoles, error) {
	rows, err := store.New(s.db).ListOwnedLogins(r.Context(), p.ID)
	if err != nil {
		return gen.ManageRoles{}, err
	}
	out := gen.ManageRoles{Enabled: p.ManageRoles, Logins: make([]struct {
		Name     string    `json:"name"`
		SyncedAt time.Time `json:"synced_at"`
	}, 0, len(rows))}
	for _, l := range rows {
		out.Logins = append(out.Logins, struct {
			Name     string    `json:"name"`
			SyncedAt time.Time `json:"synced_at"`
		}{Name: l.RoleName, SyncedAt: l.SyncedAt})
	}
	if skipped != nil {
		names := make([]string, 0, len(skipped))
		for n := range skipped {
			names = append(names, n)
		}
		slices.Sort(names)
		list := make([]struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		}, 0, len(names))
		for _, n := range names {
			list = append(list, struct {
				Name   string `json:"name"`
				Reason string `json:"reason"`
			}{Name: n, Reason: skipped[n]})
		}
		out.Skipped = &list
	}
	return out, nil
}

// GetProjectManageRoles implements GET /api/v1/projects/{id}/manage-roles.
func (s *Server) GetProjectManageRoles(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	p, err := s.projects.Get(r.Context(), id)
	if err != nil {
		s.provisionError(w, "manage roles", err)
		return
	}
	out, err := s.manageRolesOut(r, p, nil)
	if err != nil {
		s.internalError(w, "manage roles", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// SetProjectManageRoles implements PUT /api/v1/projects/{id}/manage-roles:
// organisation owners, after step-up.
func (s *Server) SetProjectManageRoles(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	var req gen.ManageRolesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.projectID = id
	a.set("enabled", req.Enabled)
	p, err := s.projects.Get(r.Context(), id)
	if err != nil {
		s.provisionError(w, "manage roles", err)
		return
	}
	p, err = ds.SetManageRoles(r.Context(), p, req.Enabled)
	if err != nil {
		s.provisionError(w, "manage roles", err)
		return
	}
	out, err := s.manageRolesOut(r, p, nil)
	if err != nil {
		s.internalError(w, "manage roles", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// SyncProjectManageRoles implements POST /api/v1/projects/{id}/manage-roles/sync.
func (s *Server) SyncProjectManageRoles(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	ds := s.dedicatedSvc(w)
	if ds == nil || !s.requireProjects(w) {
		return
	}
	p, err := s.projects.Get(r.Context(), id)
	if err != nil {
		s.provisionError(w, "manage roles", err)
		return
	}
	if p.Tier != provision.TierDedicated {
		writeError(w, http.StatusConflict, "conflict", "role management is for dedicated projects")
		return
	}
	res, err := ds.SyncOwnedRoles(r.Context(), p)
	if err != nil {
		s.provisionError(w, "manage roles", err)
		return
	}
	out, err := s.manageRolesOut(r, p, res.Skipped)
	if err != nil {
		s.internalError(w, "manage roles", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
