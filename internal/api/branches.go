package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/branching"
	"github.com/israel-duff/xenibase/internal/services"
	"github.com/israel-duff/xenibase/internal/store"
)

// ---- Branches (V2 §8) -----------------------------------------------------------

func (s *Server) requireBranches(w http.ResponseWriter) bool {
	if s.branches == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "branching is not available on this server")
		return false
	}
	return true
}

func (s *Server) branchError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, branching.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, branching.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, branching.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	default:
		s.provisionError(w, what, err)
	}
}

// tokenAllows is Actor.AllowsProject, where a token restricted to a
// project also covers its branches.
func tokenAllows(a authz.Actor, p store.Project) bool {
	return a.AllowsProject(p.ID) || (p.ParentProjectID != nil && a.AllowsProject(*p.ParentProjectID))
}

// branchFields adds a project's branch details to its API form.
func branchFields(gp *gen.Project, p store.Project) {
	gp.SensitiveData = &p.SensitiveData
	if p.ParentProjectID == nil {
		return
	}
	gp.ParentProjectId = p.ParentProjectID
	src := gen.BranchInfoSourceBackup
	if p.BranchSource != nil && *p.BranchSource == branching.SourceLive {
		src = gen.BranchInfoSourceLive
	}
	gp.Branch = &gen.BranchInfo{
		Source: src, SchemaOnly: p.BranchSchemaOnly != nil && *p.BranchSchemaOnly,
		ExpiresAt: p.ExpiresAt, Backups: p.BranchBackups,
	}
}

// ListBranches implements GET /api/v1/projects/{id}/branches.
func (s *Server) ListBranches(w http.ResponseWriter, r *http.Request, _ gen.ProjectID) {
	if !s.requireBranches(w) {
		return
	}
	p, err := s.tenantProject(r.Context())
	if err != nil {
		s.branchError(w, "list branches", err)
		return
	}
	rows, err := s.branches.List(r.Context(), p)
	if err != nil {
		s.internalError(w, "list branches", err)
		return
	}
	acc := accessFrom(r.Context())
	sess, _ := sessionFrom(r.Context())
	roles := s.myProjectRoles(r.Context(), acc, sess.UserID)
	out := gen.ProjectList{Items: []gen.Project{}}
	for _, b := range rows {
		role, ok := roles[b.ID]
		if !ok || !tokenAllows(acc.Actor, b) {
			continue // a branch the caller isn't a member of
		}
		gb, err := s.toAPIProject(b)
		if err != nil {
			s.internalError(w, "list branches", err)
			return
		}
		pr := gen.ProjectRole(role)
		gb.MyRole = &pr
		out.Items = append(out.Items, gb)
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateBranch implements POST /api/v1/projects/{id}/branches.
func (s *Server) CreateBranch(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireBranches(w) {
		return
	}
	var req gen.BranchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	acc := accessFrom(r.Context())
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.set("branch", req.Name)
	bp := branching.CreateParams{
		ParentID: id, Name: req.Name, SchemaOnly: req.SchemaOnly, CreatedBy: userID(r.Context()),
		CreatorRole: authz.ProjectAdmin, MayCopySensitive: acc.ProjectRole == authz.ProjectAdmin,
	}
	if acc.OrgRole == authz.OrgOwner || acc.OrgRole == authz.OrgAdmin {
		bp.CreatorRole = ""
	}
	if req.Source != nil {
		bp.Source = string(*req.Source)
	}
	if req.TtlHours != nil {
		ttl := time.Duration(*req.TtlHours) * time.Hour
		bp.TTL = &ttl
	}
	res, err := s.branches.Resolve(r.Context(), bp)
	if err != nil {
		s.branchError(w, "create branch", err)
		return
	}
	a.set("source", res.Source)
	a.set("schema_only", res.SchemaOnly)
	if s.tenancy != nil {
		size, err := store.New(s.db).LatestProjectSize(r.Context(), id)
		if err != nil {
			s.internalError(w, "create branch", err)
			return
		}
		if !s.checkQuota(w, s.tenancy.CheckCreateBranch(r.Context(), res.Parent.OrgID, size)) ||
			!s.checkQuota(w, s.tenancy.CheckOperation(r.Context(), res.Parent.OrgID)) {
			return
		}
	}
	if req.CopyFiles != nil {
		bp.CopyFiles = *req.CopyFiles
	}
	c, api, err := s.branches.Create(r.Context(), bp, res)
	if err != nil && c.Project.ID == uuid.Nil {
		s.branchError(w, "create branch", err)
		return
	}
	a.set("branch_project", c.Project.ID.String())
	if err != nil {
		// The branch is being made; only its API keys failed.
		s.log.Error("branch API keys", "branch", c.Project.ID, "err", err)
	}
	var extra *gen.BranchApi
	if api != nil {
		extra = &gen.BranchApi{Ref: api.Ref}
		if api.URL != "" {
			extra.Url = &api.URL
		}
		for _, k := range api.Keys {
			if k.Kind == services.KindPublishable {
				extra.PublishableKey = k.Key
			} else {
				extra.SecretKey = k.Key
			}
		}
	}
	s.writeCredentialsWith(w, c.Project, c.Operation, c.Password, extra)
}

// ResetBranch implements POST /api/v1/projects/{id}/reset.
func (s *Server) ResetBranch(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireBranches(w) {
		return
	}
	var req gen.BranchResetRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	// The parent is checked again in Reset: the caller must still be
	// allowed to branch it (a token restricted to it covers its branches).
	p := branching.ResetParams{BranchID: id, By: userID(r.Context()), Actor: accessFrom(r.Context()).Actor}
	if req.Source != nil {
		p.Source = string(*req.Source)
		a.set("source", p.Source)
	}
	op, err := s.branches.Reset(r.Context(), p)
	if err != nil {
		s.branchError(w, "reset branch", err)
		return
	}
	s.writeOperation(w, "reset branch", op)
}

// DetachBranch implements POST /api/v1/projects/{id}/detach.
func (s *Server) DetachBranch(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireBranches(w) {
		return
	}
	auditFrom(r.Context()).target("project", id.String())
	p, err := s.branches.Detach(r.Context(), id)
	if err != nil {
		s.branchError(w, "detach branch", err)
		return
	}
	gp, err := s.toAPIProject(p)
	if err != nil {
		s.internalError(w, "detach branch", err)
		return
	}
	writeJSON(w, http.StatusOK, gp)
}

// updateBranchFields applies the branch and sensitivity fields of a
// project update; false means it answered with an error.
func (s *Server) updateBranchFields(w http.ResponseWriter, r *http.Request, id gen.ProjectID, req gen.UpdateProjectRequest) bool {
	ctx := r.Context()
	a := auditFrom(ctx)
	if req.SensitiveData != nil {
		if _, err := store.New(s.db).SetProjectSensitiveData(ctx, store.SetProjectSensitiveDataParams{ID: id, SensitiveData: *req.SensitiveData}); err != nil {
			s.internalError(w, "update project", err)
			return false
		}
		a.set("sensitive_data", *req.SensitiveData)
	}
	if req.ExpiresAt == nil && req.NoExpiry == nil && req.BranchBackups == nil {
		return true
	}
	if !s.requireBranches(w) {
		return false
	}
	if req.ExpiresAt != nil || (req.NoExpiry != nil && *req.NoExpiry) {
		at := req.ExpiresAt
		if req.NoExpiry != nil && *req.NoExpiry {
			at = nil
		}
		if _, err := s.branches.SetExpiry(ctx, id, at); err != nil {
			s.branchError(w, "update project", err)
			return false
		}
		a.set("expires_at", at)
	}
	if req.BranchBackups != nil {
		if _, err := s.branches.SetBackups(ctx, id, *req.BranchBackups); err != nil {
			s.branchError(w, "update project", err)
			return false
		}
		a.set("branch_backups", *req.BranchBackups)
	}
	return true
}
