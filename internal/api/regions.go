package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/backup"
	"github.com/israel-duff/xenibase/internal/regions"
	"github.com/israel-duff/xenibase/internal/store"
)

func (s *Server) regionsSvc(w http.ResponseWriter) *regions.Service {
	if s.regions == nil {
		writeError(w, http.StatusNotImplemented, "not_configured", "regions aren't set up on this server")
	}
	return s.regions
}

// ListRegions implements GET /api/v1/regions: the regions open for new
// projects.
func (s *Server) ListRegions(w http.ResponseWriter, r *http.Request) {
	rs := s.regionsSvc(w)
	if rs == nil {
		return
	}
	list, err := rs.List(r.Context())
	if err != nil {
		s.internalError(w, "list regions", err)
		return
	}
	// A hidden region is listed to members of its early-access
	// organisations (a token: its own organisation only).
	sess, _ := sessionFrom(r.Context())
	q := store.New(s.db)
	mine := map[uuid.UUID]bool{}
	if orgs, err := q.ListUserOrgs(r.Context(), sess.UserID); err == nil {
		for _, o := range orgs {
			if sess.Token == nil || o.ID == sess.Token.OrgID {
				mine[o.ID] = true
			}
		}
	} else {
		s.internalError(w, "list regions", err)
		return
	}
	out := gen.RegionList{Items: []gen.Region{}}
	for _, g := range list {
		item := gen.Region{Id: g.ID, Name: g.Name, Country: g.Country, Residency: g.Residency, Home: g.ID == rs.Home()}
		if g.Status != "active" {
			ids, err := q.RegionEarlyAccessOrgs(r.Context(), g.ID)
			if err != nil {
				s.internalError(w, "list regions", err)
				return
			}
			allowed := []uuid.UUID{}
			for _, id := range ids {
				if mine[id] {
					allowed = append(allowed, id)
				}
			}
			if len(allowed) == 0 {
				continue
			}
			item.EarlyAccessOrgIds = &allowed
		}
		out.Items = append(out.Items, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) toAPIAdminRegion(g store.Region, home string, u store.RegionUsageRow, early []uuid.UUID) gen.AdminRegion {
	if early == nil {
		early = []uuid.UUID{}
	}
	return gen.AdminRegion{
		Id: g.ID, Name: g.Name, Country: g.Country, PoolerHost: g.PoolerHost, ApiDomain: g.ApiDomain, EarlyAccessOrgIds: early, Provider: g.Provider,
		Location: g.Location, StorageTargetId: g.StorageTargetID, CopyTargetId: g.CopyTargetID, FloatingIpId: g.FloatingIpID,
		Residency: g.Residency, Status: gen.AdminRegionStatus(g.Status), Home: g.ID == home, CreatedAt: g.CreatedAt,
		Nodes: int(u.Nodes), PoolerHosts: int(u.PoolerHosts), Projects: int(u.Projects), ResidencyProjects: int(u.ResidencyProjects),
	}
}

// ListAdminRegions implements GET /api/v1/admin/regions.
func (s *Server) ListAdminRegions(w http.ResponseWriter, r *http.Request) {
	rs := s.regionsSvc(w)
	if rs == nil {
		return
	}
	ctx := r.Context()
	q := store.New(s.db)
	list, err := rs.List(ctx)
	if err != nil {
		s.internalError(w, "list regions", err)
		return
	}
	usage, err := q.RegionUsage(ctx)
	if err != nil {
		s.internalError(w, "region usage", err)
		return
	}
	byID := map[string]store.RegionUsageRow{}
	for _, u := range usage {
		byID[u.ID] = u
	}
	out := gen.AdminRegionList{Items: make([]gen.AdminRegion, 0, len(list)), Copies: []struct {
		Count  int                             `json:"count"`
		Status gen.AdminRegionListCopiesStatus `json:"status"`
	}{}}
	for _, g := range list {
		early, err := rs.EarlyAccess(ctx, g.ID)
		if err != nil {
			s.internalError(w, "region early access", err)
			return
		}
		out.Items = append(out.Items, s.toAPIAdminRegion(g, rs.Home(), byID[g.ID], early))
	}
	counts, err := q.CopyStatusCounts(ctx)
	if err != nil {
		s.internalError(w, "copy status", err)
		return
	}
	for _, c := range counts {
		out.Copies = append(out.Copies, struct {
			Count  int                             `json:"count"`
			Status gen.AdminRegionListCopiesStatus `json:"status"`
		}{Count: int(c.N), Status: gen.AdminRegionListCopiesStatus(c.CopyStatus)})
	}
	writeJSON(w, http.StatusOK, out)
}

// PutAdminRegion implements PUT /api/v1/admin/regions/{region_id}.
func (s *Server) PutAdminRegion(w http.ResponseWriter, r *http.Request, regionID string) {
	rs := s.regionsSvc(w)
	if rs == nil {
		return
	}
	var req gen.AdminRegionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("region", regionID)
	in := regions.Input{
		ID: regionID, Name: req.Name, Country: valueOr(req.Country, ""), PoolerHost: valueOr(req.PoolerHost, ""), APIDomain: valueOr(req.ApiDomain, ""),
		Provider: valueOr(req.Provider, ""), Location: valueOr(req.Location, ""),
		StorageTargetID: req.StorageTargetId, CopyTargetID: req.CopyTargetId, FloatingIPID: req.FloatingIpId,
		Residency: valueOr(req.Residency, false), Hidden: valueOr(req.Hidden, false),
	}
	a.set("residency", in.Residency)
	a.set("pooler_host", in.PoolerHost)
	a.set("api_domain", in.APIDomain)
	if req.EarlyAccessOrgIds != nil {
		in.EarlyAccess = *req.EarlyAccessOrgIds
		a.set("early_access_org_ids", in.EarlyAccess)
	}
	g, err := rs.Save(r.Context(), in)
	switch {
	case errors.Is(err, regions.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	case errors.Is(err, regions.ErrNotReady):
		writeError(w, http.StatusConflict, "region_not_ready", err.Error())
		return
	case err != nil:
		s.internalError(w, "save region", err)
		return
	}
	usage, _ := store.New(s.db).RegionUsage(r.Context())
	var u store.RegionUsageRow
	for _, x := range usage {
		if x.ID == g.ID {
			u = x
		}
	}
	early, err := rs.EarlyAccess(r.Context(), g.ID)
	if err != nil {
		s.internalError(w, "region early access", err)
		return
	}
	writeJSON(w, http.StatusOK, s.toAPIAdminRegion(g, rs.Home(), u, early))
}

// SetProjectResidency implements PUT /api/v1/projects/{id}/residency:
// organisation owners, after step-up (V3 §6.3).
func (s *Server) SetProjectResidency(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireBackups(w) {
		return
	}
	var req gen.ProjectResidencyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.projectID = id
	a.set("enabled", req.Enabled)
	p, err := s.projects.Get(r.Context(), id)
	if err != nil {
		s.provisionError(w, "project residency", err)
		return
	}
	a.set("region", p.Region)
	out, removed, err := s.backups.SetResidency(r.Context(), p, req.Enabled)
	if err != nil {
		if errors.Is(err, backup.ErrInvalid) {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		s.internalError(w, "project residency", err)
		return
	}
	a.set("removed_copies", removed)
	gp, err := s.toAPIProject(out)
	if err != nil {
		s.internalError(w, "project residency", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.ProjectResidencyResult{Project: gp, RemovedCopies: removed})
}

// GetAdminRegionReadiness implements GET /api/v1/admin/regions/{region_id}/readiness.
func (s *Server) GetAdminRegionReadiness(w http.ResponseWriter, r *http.Request, regionID string) {
	rs := s.regionsSvc(w)
	if rs == nil {
		return
	}
	checks, err := rs.Readiness(r.Context(), regionID)
	switch {
	case errors.Is(err, regions.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such region")
		return
	case err != nil:
		s.internalError(w, "region readiness", err)
		return
	}
	out := gen.RegionReadiness{Ready: len(regions.Blocking(checks)) == 0, Checks: make([]gen.RegionCheck, 0, len(checks))}
	for _, c := range checks {
		out.Checks = append(out.Checks, gen.RegionCheck{Name: c.Name, Ok: c.OK, Blocking: c.Blocking, Detail: c.Detail})
	}
	writeJSON(w, http.StatusOK, out)
}
