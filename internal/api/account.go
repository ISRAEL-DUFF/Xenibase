package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/store"
)

// GetMe implements GET /api/v1/me.
func (s *Server) GetMe(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	writeJSON(w, http.StatusOK, genUser(sess))
}

// UpdateMe implements PATCH /api/v1/me.
func (s *Server) UpdateMe(w http.ResponseWriter, r *http.Request) {
	var req gen.UpdateMeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	var name *string
	if req.Name != nil && *req.Name != "" {
		if len(*req.Name) > 100 {
			writeError(w, http.StatusBadRequest, "bad_request", "name must be at most 100 characters")
			return
		}
		name = req.Name
	}
	u, err := store.New(s.db).UpdateUserProfile(r.Context(), store.UpdateUserProfileParams{ID: sess.UserID, Name: name})
	if err != nil {
		s.internalError(w, "update profile", err)
		return
	}
	sess.Name = ""
	if u.Name != nil {
		sess.Name = *u.Name
	}
	writeJSON(w, http.StatusOK, genUser(sess))
}

// ChangePassword implements POST /api/v1/me/password.
func (s *Server) ChangePassword(w http.ResponseWriter, r *http.Request) {
	var req gen.ChangePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "change password", err)
		return
	}
	if err := s.auth.ChangePassword(r.Context(), sess, req.CurrentPassword, req.NewPassword); err != nil {
		s.authError(w, "change password", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListMySessions implements GET /api/v1/me/sessions.
func (s *Server) ListMySessions(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	rows, err := s.auth.ListSessions(r.Context(), sess.UserID)
	if err != nil {
		s.internalError(w, "list sessions", err)
		return
	}
	out := gen.SessionList{Items: make([]gen.SessionInfo, 0, len(rows))}
	for _, row := range rows {
		item := gen.SessionInfo{Id: row.ID, CreatedAt: row.CreatedAt, LastSeenAt: row.LastSeenAt, UserAgent: row.UserAgent, Current: row.ID == sess.ID}
		if row.Ip != nil {
			ip := row.Ip.String()
			item.Ip = &ip
		}
		out.Items = append(out.Items, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// RevokeMySession implements DELETE /api/v1/me/sessions/{session_id}.
func (s *Server) RevokeMySession(w http.ResponseWriter, r *http.Request, sessionID string) {
	sess, _ := sessionFrom(r.Context())
	ok, err := s.auth.RevokeSession(r.Context(), sess.UserID, sessionID)
	if err != nil {
		s.internalError(w, "revoke session", err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return
	}
	if sessionID == sess.ID {
		s.clearSessionCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetRecoveryCodes implements GET /api/v1/me/recovery-codes.
func (s *Server) GetRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	n, err := s.auth.RecoveryCodesLeft(r.Context(), sess.UserID)
	if err != nil {
		s.internalError(w, "recovery codes", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.RecoveryCodesStatus{Remaining: n})
}

// GetTotpStatus implements GET /api/v1/me/totp.
func (s *Server) GetTotpStatus(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	st, err := s.auth.TOTPStatus(r.Context(), sess.UserID)
	if err != nil {
		s.internalError(w, "two-factor status", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.TotpStatus{Enabled: st.Enabled, Required: st.Required})
}

// SetupTotp implements POST /api/v1/me/totp/setup.
func (s *Server) SetupTotp(w http.ResponseWriter, r *http.Request) {
	var req gen.TotpSetupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "two-factor setup", err)
		return
	}
	enr, err := s.auth.BeginEnableTOTP(r.Context(), sess.UserID, req.Password)
	if err != nil {
		s.authError(w, "two-factor setup", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.TotpSetup{SetupToken: enr.Token, TotpSecret: enr.Secret, TotpUri: enr.URI, ExpiresAt: enr.Expires})
}

// EnableTotp implements POST /api/v1/me/totp/enable.
func (s *Server) EnableTotp(w http.ResponseWriter, r *http.Request) {
	var req gen.TotpEnableRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "enable two-factor", err)
		return
	}
	codes, err := s.auth.EnableTOTP(r.Context(), sess.UserID, req.SetupToken, req.Code)
	if err != nil {
		s.authError(w, "enable two-factor", err)
		return
	}
	auditFrom(r.Context()).set("two_factor", "on")
	writeJSON(w, http.StatusOK, gen.RecoveryCodes{Codes: codes})
}

// DisableTotp implements POST /api/v1/me/totp/disable.
func (s *Server) DisableTotp(w http.ResponseWriter, r *http.Request) {
	var req gen.TotpDisableRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.auth.Allow(ipString(r.Context())); err != nil {
		s.authError(w, "disable two-factor", err)
		return
	}
	if err := s.auth.DisableTOTP(r.Context(), sess.UserID, req.Password, req.Code); err != nil {
		s.authError(w, "disable two-factor", err)
		return
	}
	auditFrom(r.Context()).set("two_factor", "off")
	w.WriteHeader(http.StatusNoContent)
}

// RegenerateRecoveryCodes implements POST /api/v1/me/recovery-codes.
func (s *Server) RegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	codes, err := s.auth.RegenerateRecoveryCodes(r.Context(), sess.UserID)
	if err != nil {
		s.authError(w, "recovery codes", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.RecoveryCodes{Codes: codes})
}

// AcceptTerms implements POST /api/v1/me/terms/accept.
func (s *Server) AcceptTerms(w http.ResponseWriter, r *http.Request) {
	var req gen.AcceptTermsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	auditFrom(r.Context()).set("version", req.Version)
	if err := s.auth.AcceptTerms(r.Context(), sess.UserID, req.Version, ipFrom(r.Context())); err != nil {
		s.authError(w, "accept terms", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListMyInvitations implements GET /api/v1/me/invitations.
func (s *Server) ListMyInvitations(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	rows, err := store.New(s.db).ListInvitationsForEmail(r.Context(), sess.Email)
	if err != nil {
		s.internalError(w, "list invitations", err)
		return
	}
	out := gen.MyInvitationList{Items: make([]gen.Invitation, 0, len(rows))}
	for _, row := range rows {
		inv := genInvitation(store.Invitation{
			ID: row.ID, Email: row.Email, Kind: row.Kind, OrgID: row.OrgID, OrgRole: row.OrgRole,
			ProjectRoles: row.ProjectRoles, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt,
		}, row.InviterEmail)
		inv.OrgName = row.OrgName
		out.Items = append(out.Items, inv)
	}
	writeJSON(w, http.StatusOK, out)
}

// AcceptMyInvitation implements POST /api/v1/me/invitations/{invitation_id}/accept.
func (s *Server) AcceptMyInvitation(w http.ResponseWriter, r *http.Request, id gen.InvitationID) {
	sess, _ := sessionFrom(r.Context())
	a := auditFrom(r.Context())
	a.target("invitation", id.String())
	org, err := s.orgs.AcceptID(r.Context(), id, sess.UserID)
	if err != nil {
		s.orgError(w, "accept invitation", err)
		return
	}
	if org != nil {
		a.orgID = *org
	}
	writeJSON(w, http.StatusOK, gen.AcceptInvitationResult{OrgId: org})
}

// PreviewInvitation implements POST /api/v1/invitations/preview.
func (s *Server) PreviewInvitation(w http.ResponseWriter, r *http.Request) {
	var req gen.TokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.auth.AllowToken(ipString(r.Context())); err != nil {
		s.authError(w, "invitation", err)
		return
	}
	p, err := s.orgs.PreviewToken(r.Context(), req.Token)
	if err != nil {
		s.orgError(w, "invitation", err)
		return
	}
	inv := p.Invitation
	out := gen.InvitationPreview{
		Email: inv.Email, Kind: gen.InvitationPreviewKind(inv.Kind), OrgName: inv.OrgName,
		InvitedBy: inv.InviterEmail, HasAccount: p.HasAccount, ExpiresAt: inv.ExpiresAt,
	}
	if inv.InviterName != nil && *inv.InviterName != "" {
		out.InvitedBy = *inv.InviterName + " (" + inv.InviterEmail + ")"
	}
	if inv.OrgRole != nil {
		role := gen.OrgRole(*inv.OrgRole)
		out.Role = &role
	}
	n := len(p.ProjectRole)
	out.ProjectCount = &n
	writeJSON(w, http.StatusOK, out)
}

// AcceptInvitation implements POST /api/v1/invitations/accept.
func (s *Server) AcceptInvitation(w http.ResponseWriter, r *http.Request) {
	var req gen.AcceptInvitationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.auth.AllowToken(ipString(r.Context())); err != nil {
		s.authError(w, "invitation", err)
		return
	}
	a := auditFrom(r.Context())
	var acc *orgs.NewAccount
	sess, haveSession := sessionFrom(r.Context())
	if !haveSession && req.Password != nil {
		acc = &orgs.NewAccount{Password: *req.Password}
		if req.Name != nil {
			acc.Name = *req.Name
		}
		if req.TermsVersion != nil {
			acc.TermsVersion = *req.TermsVersion
		}
	}
	var signedIn *uuid.UUID
	if haveSession {
		signedIn = &sess.UserID
	}
	u, org, err := s.orgs.AcceptToken(r.Context(), req.Token, signedIn, acc, ipFrom(r.Context()))
	if err != nil {
		s.orgError(w, "accept invitation", err)
		return
	}
	a.userID = &u.ID
	if org != nil {
		a.orgID = *org
	}
	out := gen.AcceptInvitationResult{OrgId: org}
	if !haveSession && acc != nil {
		// Straight on to enrolling the authenticator.
		ch, err := s.auth.Login(r.Context(), u.Email, acc.Password)
		if err == nil {
			if lc, err := s.loginResult(w, r, ch); err == nil {
				out.Login = &lc
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// orgError maps organisation errors to responses.
func (s *Server) orgError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, orgs.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found, expired, or already used")
	case errors.Is(err, orgs.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	case errors.Is(err, orgs.ErrLastOwner):
		writeError(w, http.StatusConflict, "last_owner", err.Error())
	case errors.Is(err, orgs.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, orgs.ErrSignInFirst):
		writeError(w, http.StatusUnauthorized, "sign_in_to_accept", err.Error())
	case errors.Is(err, orgs.ErrWrongAccount):
		writeError(w, http.StatusForbidden, "wrong_account", err.Error())
	case errors.Is(err, orgs.ErrInvalid):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		s.authError(w, what, err)
	}
}
