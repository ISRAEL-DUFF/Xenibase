package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// ListProjects implements GET /api/v1/projects.
func (s *Server) ListProjects(w http.ResponseWriter, r *http.Request, params gen.ListProjectsParams) {
	if !s.requireProjects(w) {
		return
	}
	limit := 100
	if params.Limit != nil {
		if *params.Limit < 1 || *params.Limit > 500 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be between 1 and 500")
			return
		}
		limit = *params.Limit
	}
	var status *string
	if params.Status != nil {
		if !params.Status.Valid() {
			writeError(w, http.StatusBadRequest, "bad_request", "unknown status")
			return
		}
		st := string(*params.Status)
		status = &st
	}
	acc := accessFrom(r.Context())
	seeAll := acc.OrgRole == authz.OrgOwner || acc.OrgRole == authz.OrgAdmin
	sess, _ := sessionFrom(r.Context())
	ps, err := store.New(s.db).ListOrgProjects(r.Context(), store.ListOrgProjectsParams{
		OrgID: acc.OrgID, Status: status, SeeAll: seeAll, UserID: sess.UserID, MaxRows: int32(limit),
	})
	if err != nil {
		s.internalError(w, "list projects", err)
		return
	}
	out := gen.ProjectList{Items: make([]gen.Project, 0, len(ps))}
	last := s.lastBackups(r.Context())
	insts := s.instanceSummaries(r.Context())
	roles := s.myProjectRoles(r.Context(), acc, sess.UserID)
	counts := map[uuid.UUID]int{}
	for _, p := range ps {
		if p.ParentProjectID != nil {
			counts[*p.ParentProjectID]++
		}
	}
	for _, p := range ps {
		if !tokenAllows(acc.Actor, p) {
			continue // outside a restricted token's projects
		}
		gp, err := s.toAPIProject(p)
		if err != nil {
			s.internalError(w, "list projects", err)
			return
		}
		if role, ok := roles[p.ID]; ok {
			pr := gen.ProjectRole(role)
			gp.MyRole = &pr
		}
		if t, ok := last[p.ID]; ok {
			gp.LastBackupAt = &t
		}
		if i, ok := insts[p.InstanceID]; ok {
			gp.Instance = &i
		}
		if n, ok := counts[p.ID]; ok {
			gp.BranchCount = &n
		}
		out.Items = append(out.Items, gp)
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateProject implements POST /api/v1/projects.
func (s *Server) CreateProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireProjects(w) {
		return
	}
	var req gen.CreateProjectRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	acc, ok := s.authorizeOrg(w, r, req.OrgId, authz.OrgCreateProject)
	if !ok {
		return
	}
	// Choosing the node is placement, the platform's: a tenant could
	// otherwise put instances next to anyone's.
	if req.NodeId != nil && !acc.Actor.PlatformAdmin {
		writeError(w, http.StatusForbidden, "forbidden", "only platform admins choose the node; leave node_id out")
		return
	}
	a := auditFrom(r.Context())
	a.set("name", req.Name)
	cp := provision.CreateParams{OrgID: acc.OrgID, CreatorRole: creatorRole(acc),
		Name: req.Name, Description: req.Description, CreatedBy: userID(r.Context()), NodeID: req.NodeId}
	if o, err := store.New(s.db).GetOrg(r.Context(), acc.OrgID); err == nil {
		if set, err := store.DecodeOrgSettings(o.Settings); err == nil {
			cp.Sensitive = set.SensitiveByDefault // V2 §8.5
		}
	}
	if req.Tier != nil {
		cp.Tier = string(*req.Tier)
		a.set("tier", cp.Tier)
	}
	if req.Profile != nil {
		cp.Profile = *req.Profile
	}
	cp.Preview = req.Preview != nil && *req.Preview
	if req.PgVersion != nil {
		cp.PgVersion = *req.PgVersion
	}
	if req.VolumeGb != nil {
		cp.VolumeGB = *req.VolumeGb
	}
	if req.Region != nil {
		cp.Region = *req.Region
		a.set("region", cp.Region)
	}
	if req.DataResidency != nil && *req.DataResidency {
		cp.DataResidency = true
		a.set("data_residency", true)
	}
	if s.tenancy != nil {
		if !s.checkQuota(w, s.tenancy.CheckCreateProject(r.Context(), acc.OrgID)) {
			return
		}
		if cp.Tier == provision.TierDedicated && !s.withinAllowance(w, r, acc.OrgID, profileSize(cp.Profile, cp.VolumeGB)) {
			return
		}
	}
	c, err := s.projects.Create(r.Context(), cp)
	if err != nil {
		s.provisionError(w, "create project", err)
		return
	}
	a.target("project", c.Project.ID.String())
	a.projectID = c.Project.ID
	a.set("db_name", c.Project.DbName)
	s.writeCredentials(w, c.Project, c.Operation, c.Password)
}

// GetProject implements GET /api/v1/projects/{id}.
func (s *Server) GetProject(w http.ResponseWriter, r *http.Request, _ gen.ProjectID) {
	if !s.requireProjects(w) {
		return
	}
	p, err := s.tenantProject(r.Context())
	if err != nil || p.DeletedAt != nil {
		s.provisionError(w, "get project", provision.ErrNotFound)
		return
	}
	gp, err := s.toAPIProject(p)
	if err != nil {
		s.internalError(w, "get project", err)
		return
	}
	if role := accessFrom(r.Context()).ProjectRole; role != "" {
		pr := gen.ProjectRole(role)
		gp.MyRole = &pr
	}
	if t, ok := s.lastBackups(r.Context())[p.ID]; ok {
		gp.LastBackupAt = &t
	}
	if i, ok := s.instanceSummaries(r.Context())[p.InstanceID]; ok {
		gp.Instance = &i
	}
	if n, err := store.New(s.db).CountLiveBranches(r.Context(), store.CountLiveBranchesParams{ParentProjectID: &p.ID, OrgID: p.OrgID}); err == nil && n > 0 {
		bn := int(n)
		gp.BranchCount = &bn
	}
	if rc, err := store.New(s.db).LiveRetiredForProject(r.Context(), p.ID); err == nil {
		gp.RetiredCopyUntil = &rc.DropAfter
	}
	if s.backups != nil && s.backups.Dedicated != nil && p.Tier == provision.TierDedicated {
		if w, ok, err := s.backups.Dedicated.PITRWindow(r.Context(), p); err == nil && ok {
			gp.PitrWindow = &gen.PitrWindow{From: w.From, To: w.To}
		}
	}
	writeJSON(w, http.StatusOK, gp)
}

// DeleteProject implements DELETE /api/v1/projects/{id}.
func (s *Server) DeleteProject(w http.ResponseWriter, r *http.Request, id gen.ProjectID, params gen.DeleteProjectParams) {
	if !s.requireProjects(w) {
		return
	}
	auditFrom(r.Context()).target("project", id.String())
	skip := params.SkipFinalBackup != nil && *params.SkipFinalBackup
	auditFrom(r.Context()).set("skip_final_backup", skip)
	op, err := s.projects.Delete(r.Context(), id, params.Confirm, skip, userID(r.Context()))
	if err != nil {
		s.provisionError(w, "delete project", err)
		return
	}
	o, err := toAPIOperation(op)
	if err != nil {
		s.internalError(w, "delete project", err)
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+op.ID.String())
	writeJSON(w, http.StatusAccepted, o)
}

// RotateProjectPassword implements POST /api/v1/projects/{id}/rotate-password.
func (s *Server) RotateProjectPassword(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireProjects(w) {
		return
	}
	auditFrom(r.Context()).target("project", id.String())
	rot, err := s.projects.Rotate(r.Context(), id, userID(r.Context()))
	if err != nil {
		s.provisionError(w, "rotate password", err)
		return
	}
	s.writeCredentials(w, rot.Project, rot.Operation, rot.Password)
}

func (s *Server) writeCredentials(w http.ResponseWriter, p store.Project, op store.Operation, password string) {
	s.writeCredentialsWith(w, p, op, password, nil)
}

// writeCredentialsWith is writeCredentials with a branch's API keys.
func (s *Server) writeCredentialsWith(w http.ResponseWriter, p store.Project, op store.Operation, password string, api *gen.BranchApi) {
	gp, err := s.toAPIProject(p)
	if err != nil {
		s.internalError(w, "project credentials", err)
		return
	}
	o, err := toAPIOperation(op)
	if err != nil {
		s.internalError(w, "project credentials", err)
		return
	}
	conn := s.projects.ConnectionFor(p)
	w.Header().Set("Location", "/api/v1/operations/"+op.ID.String())
	writeJSON(w, http.StatusAccepted, gen.ProjectCredentials{
		Project:    gp,
		Operation:  o,
		Password:   password,
		Connection: toAPIConnection(conn, password),
		Api:        api,
	})
}

func (s *Server) toAPIProject(p store.Project) (gen.Project, error) {
	set, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		return gen.Project{}, err
	}
	gp := gen.Project{
		Id:          p.ID,
		OrgId:       p.OrgID,
		Name:        p.Name,
		Slug:        p.Slug,
		DbName:      store.ClientDBName(p),
		OwnerRole:   p.OwnerRole,
		Tier:        gen.ProjectTier(p.Tier),
		Status:      gen.ProjectStatus(p.Status),
		Description: p.Description,
		CreatedAt:   p.CreatedAt,
		Settings: gen.ProjectSettings{
			ConnectionLimit:                 set.ConnectionLimit,
			PoolSize:                        set.PoolSize,
			StatementTimeout:                set.StatementTimeout,
			IdleInTransactionSessionTimeout: set.IdleInTransactionTimeout,
			DiskWarnBytes:                   set.DiskWarnBytes,
			ConsoleReadOnly:                 set.ConsoleReadOnly,
			BackupRetention:                 ptrTo(gen.BackupRetention(set.Retention())),
		},
		Connection:             toAPIConnection(s.projects.ConnectionFor(p), ""),
		Region:                 &p.Region,
		DataResidency:          &p.DataResidency,
		ManageRoles:            &p.ManageRoles,
		AllowedCidrs:           allowedOrEmpty(p.AllowedCidrs),
		ForwardRegion:          p.ForwardRegion,
		ForwardUntil:           p.ForwardUntil,
		StorageState:           ptrTo(gen.StorageState(p.StorageState)),
		CanSwitchCredentials:   ptrTo(provision.CanSwitchCredentials(p)),
		LegacyCredentialsUntil: p.LegacyUntil,
		Lifecycle:              ptrTo(gen.ProjectLifecycle(p.Lifecycle)),
		LastActiveAt:           p.LastActiveAt,
		PausedAt:               p.PausedAt,
		ArchivedAt:             p.ArchivedAt,
	}
	branchFields(&gp, p)
	return gp, nil
}

func toAPIConnection(c provision.Connection, password string) gen.ConnectionInfo {
	return gen.ConnectionInfo{
		Host:        c.Host,
		SessionPort: c.SessionPort,
		PooledPort:  c.PooledPort,
		Database:    c.Database,
		User:        c.User,
		Sslmode:     c.SSLMode,
		PooledUrl:   c.PooledURL(password),
		SessionUrl:  c.SessionURL(password),
	}
}

// myProjectRoles returns userID's effective role on each project of acc's
// organisation that they can see.
func (s *Server) myProjectRoles(ctx context.Context, acc access, userID uuid.UUID) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	if acc.OrgRole == authz.OrgOwner || acc.OrgRole == authz.OrgAdmin {
		rows, err := store.New(s.db).ListOrgProjects(ctx, store.ListOrgProjectsParams{OrgID: acc.OrgID, SeeAll: true, UserID: userID, MaxRows: 10000})
		if err == nil {
			for _, p := range rows {
				out[p.ID] = authz.ProjectAdmin
			}
		}
		return out
	}
	rows, err := store.New(s.db).ListOrgProjectMemberships(ctx, acc.OrgID)
	if err == nil {
		for _, m := range rows {
			if m.UserID == userID {
				out[m.ProjectID] = m.Role
			}
		}
	}
	return out
}

func (s *Server) requireProjects(w http.ResponseWriter) bool {
	if s.projects == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"project provisioning is not configured (set XENIBASE_POOLER_CONFIG_DIR and the pooler settings)")
		return false
	}
	return true
}

func (s *Server) provisionError(w http.ResponseWriter, what string, err error) {
	// A quota counted again as the create was recorded (tenancy's claim).
	var qe *tenancy.QuotaError
	switch {
	case errors.As(err, &qe):
		writeQuotaError(w, qe)
	case errors.Is(err, provision.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, provision.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "project not found")
	case errors.Is(err, provision.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, provision.ErrNoCapacity):
		msg := err.Error()
		if err == provision.ErrNoCapacity { //nolint:errorlint // the bare sentinel gets the friendly text
			msg = "no shared cluster is available for new projects"
		}
		writeError(w, http.StatusServiceUnavailable, "no_capacity", msg)
	case errors.Is(err, dedicated.ErrHostPending):
		writeError(w, http.StatusConflict, "capacity_pending_approval", err.Error()+"; you'll be able to create the project once it is in service")
	case errors.Is(err, provision.ErrNoDedicated), errors.Is(err, provision.ErrUnreachable):
		writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
	default:
		s.internalError(w, what, err)
	}
}

func provisionUpdate(req gen.UpdateProjectRequest) provision.UpdateParams {
	p := provision.UpdateParams{Name: req.Name, Description: req.Description}
	if st := req.Settings; st != nil {
		p.Settings = &provision.SettingsPatch{
			ConnectionLimit:          st.ConnectionLimit,
			PoolSize:                 st.PoolSize,
			StatementTimeout:         st.StatementTimeout,
			IdleInTransactionTimeout: st.IdleInTransactionSessionTimeout,
			DiskWarnBytes:            st.DiskWarnBytes,
			ConsoleReadOnly:          st.ConsoleReadOnly,
		}
		if st.BackupRetention != nil {
			v := string(*st.BackupRetention)
			p.Settings.BackupRetention = &v
		}
	}
	return p
}

// networkOut is a project's IP allowlist as the API shows it.
func (s *Server) networkOut(r *http.Request, cidrs []string) gen.ProjectNetwork {
	if cidrs == nil {
		cidrs = []string{}
	}
	out := gen.ProjectNetwork{AllowedCidrs: cidrs}
	if ip := s.clientIP(r); ip.IsValid() {
		str, ok := ip.Unmap().String(), provision.AllowedFrom(cidrs, ip)
		out.YourIp, out.YourIpAllowed = &str, &ok
	}
	return out
}

// GetProjectNetwork implements GET /api/v1/projects/{id}/network.
func (s *Server) GetProjectNetwork(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	p, err := s.projects.Get(r.Context(), id)
	if err != nil {
		s.provisionError(w, "project network", err)
		return
	}
	writeJSON(w, http.StatusOK, s.networkOut(r, p.AllowedCidrs))
}

// SetProjectNetwork implements PUT /api/v1/projects/{id}/network: the
// project's IP allowlist, applied to every pooler of its region.
func (s *Server) SetProjectNetwork(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	var req gen.ProjectNetworkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	cidrs, err := provision.NormalizeAllowedCIDRs(req.AllowedCidrs)
	if err != nil {
		s.provisionError(w, "project network", err)
		return
	}
	a := auditFrom(r.Context())
	a.target("project", id.String())
	a.projectID = id
	p, err := s.projects.Get(r.Context(), id)
	if err != nil {
		s.provisionError(w, "project network", err)
		return
	}
	a.set("before", p.AllowedCidrs)
	a.set("allowed_cidrs", cidrs)
	p, err = store.New(s.db).SetProjectAllowedCIDRs(r.Context(), store.SetProjectAllowedCIDRsParams{ID: id, AllowedCidrs: cidrs})
	if err != nil {
		s.internalError(w, "project network", err)
		return
	}
	if pm := s.projects.Pooler(); pm != nil {
		if err := pm.Sync(r.Context()); err != nil {
			s.internalError(w, "project network: pooler sync", err)
			return
		}
		// HBA rules apply at login: drop clients already connected from
		// addresses the list no longer admits.
		n, err := pm.DropDisallowed(r.Context(), p, p.AllowedCidrs)
		if err != nil {
			s.log.Warn("project network: drop disallowed clients", "project_id", p.ID, "err", err)
		}
		if n > 0 {
			a.set("dropped_connections", n)
		}
	}
	writeJSON(w, http.StatusOK, s.networkOut(r, p.AllowedCidrs))
}

func allowedOrEmpty(c []string) *[]string {
	if c == nil {
		c = []string{}
	}
	return &c
}
