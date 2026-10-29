package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/store"
)

func (s *Server) genOrg(r *http.Request, o store.Organization, role string, members, projects int) (gen.Org, error) {
	sess, _ := sessionFrom(r.Context())
	set, err := store.DecodeOrgSettings(o.Settings)
	if err != nil {
		return gen.Org{}, err
	}
	plan := ""
	if p, err := s.planName(r, o.PlanID); err == nil {
		plan = p
	}
	g := gen.Org{
		Id: o.ID, Name: o.Name, Slug: o.Slug, Personal: o.PersonalOwnerID != nil && *o.PersonalOwnerID == sess.UserID,
		Role: gen.OrgRole(role), Plan: plan, Status: gen.OrgStatus(o.Status),
		MembersCanCreateProjects: set.MembersCanCreateProjects, SensitiveByDefault: &set.SensitiveByDefault, MemberCount: members, ProjectCount: projects, CreatedAt: o.CreatedAt,
		SuspendedReason: o.SuspendedReason, DeleteAfter: o.DeleteAfter,
	}
	// Everyone in the organisation sees open break-glass sessions (V2 §2.4).
	bgs, err := store.New(s.db).OrgActiveBreakGlass(r.Context(), o.ID)
	if err != nil {
		return g, err
	}
	sessions := make([]gen.BreakGlassSession, 0, len(bgs))
	for _, b := range bgs {
		sessions = append(sessions, gen.BreakGlassSession{Id: b.ID, OrgId: b.OrgID, AdminEmail: b.AdminEmail, Reason: b.Reason, StartsAt: b.StartsAt, ExpiresAt: b.ExpiresAt})
	}
	g.BreakGlass = &sessions
	return g, s.billingStanding(r, &g, role)
}

func (s *Server) planName(r *http.Request, id uuid.UUID) (string, error) {
	var name string
	err := s.db.QueryRow(r.Context(), `SELECT name FROM quota_plans WHERE id = $1`, id).Scan(&name)
	return name, err
}

// ListOrgs implements GET /api/v1/orgs.
func (s *Server) ListOrgs(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	rows, err := store.New(s.db).ListUserOrgs(r.Context(), sess.UserID)
	if err != nil {
		s.internalError(w, "list orgs", err)
		return
	}
	out := gen.OrgList{Items: make([]gen.Org, 0, len(rows))}
	for _, row := range rows {
		if sess.Token != nil && row.ID != sess.Token.OrgID {
			continue // a token sees only its organisation
		}
		o := store.Organization{ID: row.ID, Name: row.Name, Slug: row.Slug, PersonalOwnerID: row.PersonalOwnerID,
			PlanID: row.PlanID, Settings: row.Settings, Status: row.Status, CreatedAt: row.CreatedAt,
			SuspendedReason: row.SuspendedReason, DeleteAfter: row.DeleteAfter}
		g, err := s.genOrg(r, o, row.MemberRole, int(row.MemberCount), int(row.ProjectCount))
		if err != nil {
			s.internalError(w, "list orgs", err)
			return
		}
		out.Items = append(out.Items, g)
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateOrg implements POST /api/v1/orgs.
func (s *Server) CreateOrg(w http.ResponseWriter, r *http.Request) {
	var req gen.CreateOrgRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	o, err := s.orgs.Create(r.Context(), sess.UserID, req.Name)
	if err != nil {
		s.orgError(w, "create org", err)
		return
	}
	a := auditFrom(r.Context())
	a.orgID = o.ID
	a.target("org", o.ID.String())
	g, err := s.genOrg(r, o, authz.OrgOwner, 1, 0)
	if err != nil {
		s.internalError(w, "create org", err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (s *Server) orgResponse(w http.ResponseWriter, r *http.Request, orgID uuid.UUID, status int) {
	acc := accessFrom(r.Context())
	sess, _ := sessionFrom(r.Context())
	rows, err := store.New(s.db).ListUserOrgs(r.Context(), sess.UserID)
	if err != nil {
		s.internalError(w, "get org", err)
		return
	}
	for _, row := range rows {
		if row.ID != orgID {
			continue
		}
		o := store.Organization{ID: row.ID, Name: row.Name, Slug: row.Slug, PersonalOwnerID: row.PersonalOwnerID,
			PlanID: row.PlanID, Settings: row.Settings, Status: row.Status, CreatedAt: row.CreatedAt,
			SuspendedReason: row.SuspendedReason, DeleteAfter: row.DeleteAfter}
		g, err := s.genOrg(r, o, acc.OrgRole, int(row.MemberCount), int(row.ProjectCount))
		if err != nil {
			s.internalError(w, "get org", err)
			return
		}
		writeJSON(w, status, g)
		return
	}
	if acc.BreakGlass {
		// A platform admin in a break-glass session is not a member (V2 §2.4).
		o, err := store.New(s.db).GetOrg(r.Context(), orgID)
		if err != nil {
			s.internalError(w, "get org", err)
			return
		}
		var members, projects int
		_ = s.db.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM org_members WHERE org_id = $1),
			(SELECT count(*) FROM projects WHERE org_id = $1 AND deleted_at IS NULL)`, orgID).Scan(&members, &projects)
		g, err := s.genOrg(r, o, acc.OrgRole, members, projects)
		if err != nil {
			s.internalError(w, "get org", err)
			return
		}
		writeJSON(w, status, g)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "not found")
}

// GetOrg implements GET /api/v1/orgs/{org}.
func (s *Server) GetOrg(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	s.orgResponse(w, r, org, http.StatusOK)
}

// UpdateOrg implements PATCH /api/v1/orgs/{org}.
func (s *Server) UpdateOrg(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	var req gen.UpdateOrgRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	auditFrom(r.Context()).target("org", org.String())
	if _, err := s.orgs.Update(r.Context(), org, orgs.Patch{Name: req.Name, Slug: req.Slug, MembersCanCreateProjects: req.MembersCanCreateProjects, SensitiveByDefault: req.SensitiveByDefault}); err != nil {
		s.orgError(w, "update org", err)
		return
	}
	s.orgResponse(w, r, org, http.StatusOK)
}

// ListOrgMembers implements GET /api/v1/orgs/{org}/members.
func (s *Server) ListOrgMembers(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	// OrgView lets a project-restricted token through for the organisation's
	// own record, but the member list names people beyond its projects.
	if accessFrom(r.Context()).Actor.Restricted() {
		writeError(w, http.StatusForbidden, "insufficient_scope", "this token is restricted to some projects; listing organisation members needs an unrestricted token")
		return
	}
	q := store.New(s.db)
	rows, err := q.ListOrgMembers(r.Context(), org)
	if err != nil {
		s.internalError(w, "list members", err)
		return
	}
	memberships, err := q.ListOrgProjectMemberships(r.Context(), org)
	if err != nil {
		s.internalError(w, "list members", err)
		return
	}
	byUser := map[uuid.UUID][]gen.ProjectMembership{}
	for _, m := range memberships {
		byUser[m.UserID] = append(byUser[m.UserID], gen.ProjectMembership{ProjectId: m.ProjectID, ProjectName: m.ProjectName, Role: gen.ProjectRole(m.Role)})
	}
	out := gen.OrgMemberList{Items: make([]gen.OrgMember, 0, len(rows))}
	for _, row := range rows {
		projects := byUser[row.UserID]
		if projects == nil {
			projects = []gen.ProjectMembership{}
		}
		out.Items = append(out.Items, gen.OrgMember{
			UserId: row.UserID, Email: row.Email, Name: row.Name, Role: gen.OrgRole(row.Role), TotpEnabled: row.TotpEnabled,
			Disabled: row.DisabledAt != nil, LastActiveAt: row.LastActiveAt, JoinedAt: row.CreatedAt, Projects: projects,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// refuseBreakGlass answers 403 for a membership change made through a
// break-glass session (V2 §2.4): the session is time-limited access, so it
// may not hand out roles, its admin's own included, that would outlive it.
// It writes the refusal and returns true.
func refuseBreakGlass(w http.ResponseWriter, acc access) bool {
	if !acc.BreakGlass {
		return false
	}
	writeError(w, http.StatusForbidden, "break_glass", "invitations, role changes and ownership transfers can't be made under break-glass; ask an owner of the organisation")
	return true
}

func genInvitation(inv store.Invitation, inviter string) gen.Invitation {
	out := gen.Invitation{
		Id: inv.ID, Email: inv.Email, Kind: gen.InvitationKind(inv.Kind), OrgId: inv.OrgID, InvitedBy: inviter,
		ExpiresAt: inv.ExpiresAt, CreatedAt: inv.CreatedAt, Projects: []gen.InviteProjectRole{},
	}
	if inv.OrgRole != nil {
		role := gen.OrgRole(*inv.OrgRole)
		out.Role = &role
	}
	var roles []orgs.ProjectRole
	_ = json.Unmarshal(inv.ProjectRoles, &roles)
	for _, pr := range roles {
		out.Projects = append(out.Projects, gen.InviteProjectRole{ProjectId: pr.ProjectID, Role: gen.ProjectRole(pr.Role)})
	}
	return out
}

func (s *Server) invitationCreated(in orgs.Invited, inviter string) gen.InvitationCreated {
	out := gen.InvitationCreated{Invitation: genInvitation(in.Invitation, inviter), Url: in.URL, EmailSent: in.EmailError == nil}
	if in.EmailError != nil {
		msg := in.EmailError.Error()
		out.EmailError = &msg
	}
	return out
}

// InviteOrgMember implements POST /api/v1/orgs/{org}/members.
func (s *Server) InviteOrgMember(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	var req gen.InviteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	acc := accessFrom(r.Context())
	sess, _ := sessionFrom(r.Context())
	a := auditFrom(r.Context())
	a.set("email", string(req.Email))
	a.set("role", string(req.Role))
	if refuseBreakGlass(w, acc) {
		return
	}
	if req.Role == gen.OrgRoleOwner && acc.OrgRole != authz.OrgOwner {
		writeError(w, http.StatusForbidden, "forbidden", "only owners can invite owners")
		return
	}
	if req.Role == gen.OrgRoleBilling && acc.OrgRole != authz.OrgOwner {
		// Admins can't see billing, so they can't hand out access to it.
		writeError(w, http.StatusForbidden, "forbidden", "only owners can invite billing members")
		return
	}
	p := orgs.InviteParams{OrgID: &org, Email: string(req.Email), OrgRole: string(req.Role), InvitedBy: sess.UserID, InviterName: displayName(sess.Name, sess.Email)}
	if req.Projects != nil {
		for _, pr := range *req.Projects {
			p.Projects = append(p.Projects, orgs.ProjectRole{ProjectID: pr.ProjectId, Role: string(pr.Role)})
		}
	}
	in, err := s.orgs.Invite(r.Context(), p)
	if err != nil {
		s.orgError(w, "invite", err)
		return
	}
	a.target("invitation", in.Invitation.ID.String())
	writeJSON(w, http.StatusCreated, s.invitationCreated(in, sess.Email))
}

func displayName(name, email string) string {
	if name != "" {
		return name
	}
	return email
}

// UpdateOrgMember implements PATCH /api/v1/orgs/{org}/members/{user}.
func (s *Server) UpdateOrgMember(w http.ResponseWriter, r *http.Request, org gen.OrgID, user gen.UserID) {
	var req gen.OrgRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.target("user", user.String())
	a.set("role", string(req.Role))
	acc := accessFrom(r.Context())
	if refuseBreakGlass(w, acc) {
		return
	}
	if err := s.orgs.SetMemberRole(r.Context(), org, acc.OrgRole, user, string(req.Role)); err != nil {
		s.orgError(w, "change role", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RemoveOrgMember implements DELETE /api/v1/orgs/{org}/members/{user}.
func (s *Server) RemoveOrgMember(w http.ResponseWriter, r *http.Request, org gen.OrgID, user gen.UserID) {
	auditFrom(r.Context()).target("user", user.String())
	if err := s.orgs.RemoveMember(r.Context(), org, accessFrom(r.Context()).OrgRole, user); err != nil {
		s.orgError(w, "remove member", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// LeaveOrg implements POST /api/v1/orgs/{org}/leave.
func (s *Server) LeaveOrg(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	sess, _ := sessionFrom(r.Context())
	if err := s.orgs.Leave(r.Context(), org, sess.UserID); err != nil {
		s.orgError(w, "leave", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// TransferOrgOwnership implements POST /api/v1/orgs/{org}/transfer-ownership.
func (s *Server) TransferOrgOwnership(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	var req gen.TransferOwnershipRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	auditFrom(r.Context()).target("user", req.UserId.String())
	if refuseBreakGlass(w, accessFrom(r.Context())) {
		return
	}
	if sess.Token != nil {
		o, err := store.New(s.db).GetOrg(r.Context(), org)
		if err != nil {
			s.internalError(w, "transfer ownership", err)
			return
		}
		if !tokenConfirmed(w, r, o.Name, req.Confirm) {
			return
		}
	}
	if err := s.orgs.TransferOwnership(r.Context(), org, sess.UserID, req.UserId); err != nil {
		s.orgError(w, "transfer ownership", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListOrgInvitations implements GET /api/v1/orgs/{org}/invitations.
func (s *Server) ListOrgInvitations(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	rows, err := store.New(s.db).ListOrgInvitations(r.Context(), &org)
	if err != nil {
		s.internalError(w, "list invitations", err)
		return
	}
	out := gen.InvitationList{Items: make([]gen.Invitation, 0, len(rows))}
	for _, row := range rows {
		out.Items = append(out.Items, genInvitation(store.Invitation{
			ID: row.ID, Email: row.Email, Kind: row.Kind, OrgID: row.OrgID, OrgRole: row.OrgRole,
			ProjectRoles: row.ProjectRoles, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
		}, row.InviterEmail))
	}
	writeJSON(w, http.StatusOK, out)
}

// RevokeOrgInvitation implements DELETE /api/v1/orgs/{org}/invitations/{invitation_id}.
func (s *Server) RevokeOrgInvitation(w http.ResponseWriter, r *http.Request, org gen.OrgID, id gen.InvitationID) {
	auditFrom(r.Context()).target("invitation", id.String())
	n, err := store.New(s.db).RevokeOrgInvitation(r.Context(), store.RevokeOrgInvitationParams{ID: id, OrgID: &org})
	if err != nil {
		s.internalError(w, "revoke invitation", err)
		return
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "not_found", "no pending invitation with that id")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Audit logs ----------------------------------------------------------------

type auditQuery struct {
	action, target *string
	outcome        *string
	before         *int64
	limit          *int
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, arg store.ListAuditParams, q auditQuery) {
	limit := 100
	if q.limit != nil {
		if *q.limit < 1 || *q.limit > 500 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be between 1 and 500")
			return
		}
		limit = *q.limit
	}
	arg.Action, arg.TargetID, arg.BeforeID, arg.Outcome, arg.MaxRows = q.action, q.target, q.before, q.outcome, int32(limit)
	rows, err := store.New(s.db).ListAudit(r.Context(), arg)
	if err != nil {
		s.internalError(w, "list audit", err)
		return
	}
	out := gen.AuditList{Items: make([]gen.AuditEntry, 0, len(rows))}
	for _, row := range rows {
		detail := map[string]any{}
		_ = json.Unmarshal(row.Detail, &detail)
		kind := gen.AuditEntryActorKind(row.ActorKind)
		bg := row.BreakGlass
		e := gen.AuditEntry{
			Id: row.ID, UserId: row.UserID, UserEmail: row.UserEmail, Action: row.Action, ActorKind: &kind,
			OrgId: row.OrgID, ProjectId: row.ProjectID, BreakGlass: &bg,
			TargetType: row.TargetType, TargetId: row.TargetID, Detail: detail, UserAgent: row.UserAgent,
			Outcome: gen.AuditEntryOutcome(row.Outcome), CreatedAt: row.CreatedAt,
		}
		if row.Ip != nil {
			ip := row.Ip.String()
			e.Ip = &ip
		}
		out.Items = append(out.Items, e)
	}
	if len(rows) == limit {
		next := rows[len(rows)-1].ID
		out.NextBefore = &next
	}
	writeJSON(w, http.StatusOK, out)
}

func outcomeStr[T ~string](o *T) *string {
	if o == nil {
		return nil
	}
	s := string(*o)
	return &s
}

// ListOrgAudit implements GET /api/v1/orgs/{org}/audit.
func (s *Server) ListOrgAudit(w http.ResponseWriter, r *http.Request, org gen.OrgID, p gen.ListOrgAuditParams) {
	s.listAudit(w, r, store.ListAuditParams{OrgID: &org},
		auditQuery{action: p.Action, target: p.TargetId, outcome: outcomeStr(p.Outcome), before: p.Before, limit: p.Limit})
}

// ListProjectAudit implements GET /api/v1/projects/{id}/audit.
func (s *Server) ListProjectAudit(w http.ResponseWriter, r *http.Request, id gen.ProjectID, p gen.ListProjectAuditParams) {
	acc := accessFrom(r.Context())
	s.listAudit(w, r, store.ListAuditParams{OrgID: &acc.OrgID, ProjectID: &id},
		auditQuery{action: p.Action, target: p.TargetId, outcome: outcomeStr(p.Outcome), before: p.Before, limit: p.Limit})
}

// ListPlatformAudit implements GET /api/v1/admin/audit.
func (s *Server) ListPlatformAudit(w http.ResponseWriter, r *http.Request, p gen.ListPlatformAuditParams) {
	s.listAudit(w, r, store.ListAuditParams{Platform: true},
		auditQuery{action: p.Action, target: p.TargetId, outcome: outcomeStr(p.Outcome), before: p.Before, limit: p.Limit})
}

// ---- Project members, credentials, transfer -------------------------------------

// ListProjectMembers implements GET /api/v1/projects/{id}/members.
func (s *Server) ListProjectMembers(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	acc := accessFrom(r.Context())
	q := store.New(s.db)
	explicit, err := q.ListProjectMembers(r.Context(), store.ListProjectMembersParams{ProjectID: id, OrgID: acc.OrgID})
	if err != nil {
		s.internalError(w, "list project members", err)
		return
	}
	orgMembers, err := q.ListOrgMembers(r.Context(), acc.OrgID)
	if err != nil {
		s.internalError(w, "list project members", err)
		return
	}
	logins, err := q.ListProjectDBUsers(r.Context(), id)
	if err != nil {
		s.internalError(w, "list project members", err)
		return
	}
	has := map[uuid.UUID]bool{}
	for _, l := range logins {
		has[l.UserID] = true
	}
	out := gen.ProjectMemberList{Items: []gen.ProjectMember{}}
	seen := map[uuid.UUID]bool{}
	for _, m := range orgMembers {
		if m.Role != authz.OrgOwner && m.Role != authz.OrgAdmin {
			continue
		}
		seen[m.UserID] = true
		role := gen.OrgRole(m.Role)
		h := has[m.UserID]
		out.Items = append(out.Items, gen.ProjectMember{UserId: m.UserID, Email: m.Email, Name: m.Name, Role: gen.ProjectRoleAdmin,
			OrgRole: &role, Implicit: true, HasCredentials: &h})
	}
	for _, m := range explicit {
		if seen[m.UserID] {
			continue
		}
		h := has[m.UserID]
		pm := gen.ProjectMember{UserId: m.UserID, Email: m.Email, Name: m.Name, Role: gen.ProjectRole(m.Role), HasCredentials: &h}
		if m.OrgRole != nil {
			role := gen.OrgRole(*m.OrgRole)
			pm.OrgRole = &role
		}
		out.Items = append(out.Items, pm)
	}
	writeJSON(w, http.StatusOK, out)
}

// AddProjectMember implements POST /api/v1/projects/{id}/members.
func (s *Server) AddProjectMember(w http.ResponseWriter, r *http.Request, _ gen.ProjectID) {
	var req gen.ProjectMemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	acc := accessFrom(r.Context())
	sess, _ := sessionFrom(r.Context())
	a := auditFrom(r.Context())
	a.set("email", string(req.Email))
	a.set("role", string(req.Role))
	if refuseBreakGlass(w, acc) {
		return
	}
	p, err := s.tenantProject(r.Context())
	if err != nil {
		s.internalError(w, "add member", err)
		return
	}
	q := store.New(s.db)
	if u, err := q.GetUserByEmail(r.Context(), string(req.Email)); err == nil {
		if _, err := q.GetOrgMember(r.Context(), store.GetOrgMemberParams{OrgID: acc.OrgID, UserID: u.ID}); err == nil {
			a.target("user", u.ID.String())
			if err := s.orgs.AddProjectMember(r.Context(), p, u.ID, string(req.Role), sess.UserID); err != nil {
				s.orgError(w, "add member", err)
				return
			}
			writeJSON(w, http.StatusOK, gen.ProjectMemberAdded{Added: true})
			return
		} else if !errors.Is(err, pgx.ErrNoRows) {
			s.internalError(w, "add member", err)
			return
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		s.internalError(w, "add member", err)
		return
	}
	// Not in the organisation yet: invite them as a member with this
	// project role (V2 §3.2; project admins can invite to their project).
	in, err := s.orgs.Invite(r.Context(), orgs.InviteParams{
		OrgID: &acc.OrgID, Email: string(req.Email), OrgRole: authz.OrgMember, InvitedBy: sess.UserID,
		InviterName: displayName(sess.Name, sess.Email), Projects: []orgs.ProjectRole{{ProjectID: p.ID, Role: string(req.Role)}},
	})
	if err != nil {
		s.orgError(w, "invite", err)
		return
	}
	a.target("invitation", in.Invitation.ID.String())
	created := s.invitationCreated(in, sess.Email)
	writeJSON(w, http.StatusOK, gen.ProjectMemberAdded{Added: false, Invitation: &created})
}

// UpdateProjectMember implements PATCH /api/v1/projects/{id}/members/{user}.
func (s *Server) UpdateProjectMember(w http.ResponseWriter, r *http.Request, id gen.ProjectID, user gen.UserID) {
	var req gen.ProjectRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	a := auditFrom(r.Context())
	a.target("user", user.String())
	a.set("role", string(req.Role))
	if refuseBreakGlass(w, accessFrom(r.Context())) {
		return
	}
	p, err := s.tenantProject(r.Context())
	if err != nil {
		s.internalError(w, "change member role", err)
		return
	}
	if _, err := store.New(s.db).GetProjectMember(r.Context(), store.GetProjectMemberParams{ProjectID: id, UserID: user, OrgID: p.OrgID}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "that user is not a member of this project")
		return
	} else if err != nil {
		s.internalError(w, "change member role", err)
		return
	}
	if err := s.orgs.AddProjectMember(r.Context(), p, user, string(req.Role), sess.UserID); err != nil {
		s.orgError(w, "change member role", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RemoveProjectMember implements DELETE /api/v1/projects/{id}/members/{user}.
func (s *Server) RemoveProjectMember(w http.ResponseWriter, r *http.Request, _ gen.ProjectID, user gen.UserID) {
	auditFrom(r.Context()).target("user", user.String())
	p, err := s.tenantProject(r.Context())
	if err != nil {
		s.internalError(w, "remove member", err)
		return
	}
	if err := s.orgs.RemoveProjectMember(r.Context(), p, user); err != nil {
		s.orgError(w, "remove member", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetMyCredentials implements GET /api/v1/projects/{id}/credentials.
func (s *Server) GetMyCredentials(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	acc := accessFrom(r.Context())
	sess, _ := sessionFrom(r.Context())
	out := gen.PersonalCredentialsInfo{Access: gen.PersonalCredentialsInfoAccess(authz.CredentialAccess(acc.ProjectRole))}
	l, err := store.New(s.db).GetProjectDBUser(r.Context(), store.GetProjectDBUserParams{ProjectID: id, UserID: sess.UserID, OrgID: acc.OrgID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		s.internalError(w, "credentials", err)
		return
	default:
		out.Exists, out.Role, out.CreatedAt, out.RotatedAt = true, &l.RoleName, &l.CreatedAt, l.RotatedAt
	}
	writeJSON(w, http.StatusOK, out)
}

// IssueMyCredentials implements POST /api/v1/projects/{id}/credentials.
func (s *Server) IssueMyCredentials(w http.ResponseWriter, r *http.Request, _ gen.ProjectID) {
	sess, _ := sessionFrom(r.Context())
	p, err := s.tenantProject(r.Context())
	if err != nil {
		s.internalError(w, "credentials", err)
		return
	}
	c, err := s.orgs.IssueCredentials(r.Context(), p, sess.UserID)
	if err != nil {
		s.provisionError(w, "credentials", err)
		return
	}
	a := auditFrom(r.Context())
	a.target("db_user", c.Role)
	a.set("access", c.Access)
	conn := s.projects.ConnectionFor(p)
	conn.User = c.Role
	info := toAPIConnection(conn, c.Password)
	writeJSON(w, http.StatusOK, gen.PersonalCredentials{Role: c.Role, Password: c.Password,
		Access: gen.PersonalCredentialsAccess(c.Access), Connection: info})
}

// TransferProject implements POST /api/v1/projects/{id}/transfer.
func (s *Server) TransferProject(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	var req gen.TransferProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	acc := accessFrom(r.Context())
	a := auditFrom(r.Context())
	a.set("to_org", req.OrgId.String())
	q := store.New(s.db)
	// Owner of both organisations (V2 §2.1).
	for _, org := range []uuid.UUID{acc.OrgID, req.OrgId} {
		d, err := authz.Can(r.Context(), q, acc.Actor, authz.OrgOwnerOnly, authz.Resource{OrgID: org})
		if err != nil {
			s.internalError(w, "transfer", err)
			return
		}
		if !d.Visible && org != acc.OrgID {
			writeError(w, http.StatusNotFound, "not_found", "no such organisation")
			return
		}
		if !d.Allowed {
			writeError(w, http.StatusForbidden, "forbidden", "you must own both organisations to move a project")
			return
		}
	}
	p, err := s.tenantProject(r.Context())
	if err != nil {
		s.internalError(w, "transfer", err)
		return
	}
	if err := s.orgs.Transfer(r.Context(), p, req.OrgId); err != nil {
		s.orgError(w, "transfer", err)
		return
	}
	moved, err := q.GetOrgProject(r.Context(), store.GetOrgProjectParams{ID: id, OrgID: req.OrgId})
	if err != nil {
		s.internalError(w, "transfer", err)
		return
	}
	gp, err := s.toAPIProject(moved)
	if err != nil {
		s.internalError(w, "transfer", err)
		return
	}
	writeJSON(w, http.StatusOK, gp)
}
