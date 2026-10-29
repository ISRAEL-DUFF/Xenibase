package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tokens"
)

// Default bearer rate limits (V2 §7.2: per token, and per organisation
// across its tokens).
const (
	DefaultTokenRate    = 600  // requests per token per minute
	DefaultOrgTokenRate = 1200 // requests per organisation's tokens per minute
)

// bearerToken returns the request's "Authorization: Bearer" credential.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	t := strings.TrimSpace(h[7:])
	return t, t != ""
}

// tokenSession authenticates an API token and applies its rate limits. A
// non-zero status refuses the request.
func (s *Server) tokenSession(ctx context.Context, secret string) (auth.Session, int, string, string) {
	if s.tokens == nil {
		return auth.Session{}, http.StatusUnauthorized, "invalid_token", "API tokens are not enabled"
	}
	t, err := s.tokens.Authenticate(ctx, secret, ipFrom(ctx))
	switch {
	case errors.Is(err, tokens.ErrUnauthenticated):
		return auth.Session{}, http.StatusUnauthorized, "invalid_token", "the API token is not valid: it may be revoked or expired"
	case errors.Is(err, tokens.ErrSuspended):
		return auth.Session{}, http.StatusForbidden, "org_suspended", err.Error()
	case err != nil:
		s.log.Error("authenticate token", "err", err)
		return auth.Session{}, http.StatusInternalServerError, "internal", "internal error"
	}
	if !s.tokenLimit.Allow(t.ID.String()) || !s.orgTokenLimit.Allow(t.OrgID.String()) {
		return auth.Session{}, http.StatusTooManyRequests, "rate_limited", "too many requests with this token; slow down"
	}
	return auth.Session{
		UserID: t.UserID, Email: t.Email, Name: t.UserName, PlatformRole: t.PlatformRole,
		Token: &auth.TokenGrant{ID: t.ID, OrgID: t.OrgID, Name: t.Name, Scopes: t.Scopes, Projects: t.Projects, ExpiresAt: t.ExpiresAt},
	}, 0, "", ""
}

// writeScopeError explains why authz refused an API token.
func writeScopeError(w http.ResponseWriter, need string) {
	switch need {
	case "unrestricted":
		writeError(w, http.StatusForbidden, "insufficient_scope", "this token is restricted to some projects; organisation-wide actions need an unrestricted token")
	case "session":
		writeError(w, http.StatusForbidden, "token_not_allowed", "this can't be done with an API token; use the web UI")
	default:
		writeError(w, http.StatusForbidden, "insufficient_scope", fmt.Sprintf("this needs a token with the %s scope", need))
	}
}

// tokenConfirmed enforces the typed confirmation API tokens give instead of
// step-up auth (V2 §7.2). It writes the refusal and returns false.
func tokenConfirmed(w http.ResponseWriter, r *http.Request, want string, got *string) bool {
	sess, _ := sessionFrom(r.Context())
	if sess.Token == nil {
		return true
	}
	if got == nil || *got != want {
		writeError(w, http.StatusBadRequest, "confirm_required", fmt.Sprintf("with an API token, confirm by sending \"confirm\": %q", want))
		return false
	}
	return true
}

func tokenError(s *Server, w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, tokens.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid", strings.TrimPrefix(err.Error(), tokens.ErrInvalid.Error()+": "))
	case errors.Is(err, tokens.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	default:
		s.internalError(w, what, err)
	}
}

func genToken(t store.ApiToken, now time.Time) gen.APIToken {
	out := gen.APIToken{
		Id: t.ID, Name: t.Name, OrgId: t.OrgID, Prefix: t.Prefix, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt,
		RevokedAt: t.RevokedAt, CreatedAt: t.CreatedAt, CreatedVia: gen.APITokenCreatedVia(t.CreatedVia), UserId: &t.UserID,
		Status: "active",
	}
	for _, sc := range t.Scopes {
		out.Scopes = append(out.Scopes, gen.TokenScope(sc))
	}
	if t.ProjectIds != nil {
		ids := slices.Clone(t.ProjectIds)
		out.ProjectIds = &ids
	}
	if t.LastUsedIp != nil {
		ip := t.LastUsedIp.String()
		out.LastUsedIp = &ip
	}
	switch {
	case t.RevokedAt != nil:
		out.Status = "revoked"
	case !now.Before(t.ExpiresAt):
		out.Status = "expired"
	}
	return out
}

func scopeStrings(in []gen.TokenScope) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, string(s))
	}
	return out
}

// checkTokenGrant checks that the signed-in caller may issue a token for
// orgID and projects: they can see the organisation and every project.
// With a token, the new one may not exceed it (V2 §7.2: a token never
// exceeds what created it). It writes the refusal and returns false.
func (s *Server) checkTokenGrant(w http.ResponseWriter, r *http.Request, orgID uuid.UUID, scopes []string, projects *[]uuid.UUID, expiresIn time.Duration) bool {
	ctx := r.Context()
	sess, _ := sessionFrom(ctx)
	actor := actorFor(sess)
	q := store.New(s.db)
	d, err := authz.Can(ctx, q, actor, authz.OrgView, authz.Resource{OrgID: orgID})
	if err != nil {
		s.internalError(w, "authorize", err)
		return false
	}
	if !d.Visible {
		writeError(w, http.StatusNotFound, "not_found", "no such organisation")
		return false
	}
	if projects != nil {
		for _, id := range *projects {
			pd, err := authz.Can(ctx, q, actor, authz.ProjectView, authz.Resource{OrgID: orgID, ProjectID: id})
			if err != nil {
				s.internalError(w, "authorize", err)
				return false
			}
			if !pd.Visible {
				writeError(w, http.StatusBadRequest, "invalid", fmt.Sprintf("project %s isn't one of yours in that organisation", id))
				return false
			}
		}
	}
	if t := sess.Token; t != nil {
		for _, sc := range scopes {
			if !authz.HasScope(t.Scopes, sc) {
				writeError(w, http.StatusForbidden, "insufficient_scope", "a token can't create a token with more scopes than it has")
				return false
			}
		}
		if t.Projects != nil && (projects == nil || slices.ContainsFunc(*projects, func(id uuid.UUID) bool { return !slices.Contains(t.Projects, id) })) {
			writeError(w, http.StatusForbidden, "insufficient_scope", "a project-restricted token can only create tokens for its own projects")
			return false
		}
		row, err := q.GetUserToken(ctx, store.GetUserTokenParams{ID: t.ID, UserID: sess.UserID})
		if err != nil {
			s.internalError(w, "authorize", err)
			return false
		}
		if s.now().Add(expiresIn).After(row.ExpiresAt) {
			writeError(w, http.StatusForbidden, "insufficient_scope", "a token can't create a token that outlives it")
			return false
		}
	}
	return true
}

func expiryDays(days *int) time.Duration {
	if days == nil {
		return 0
	}
	return time.Duration(*days) * 24 * time.Hour
}

// ListMyTokens implements GET /api/v1/tokens.
func (s *Server) ListMyTokens(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	rows, err := store.New(s.db).ListUserTokens(r.Context(), sess.UserID)
	if err != nil {
		s.internalError(w, "list tokens", err)
		return
	}
	now := s.now()
	out := gen.APITokenList{Items: []gen.APIToken{}}
	for _, row := range rows {
		if sess.Token != nil && row.OrgID != sess.Token.OrgID {
			continue // a token sees only its own organisation's tokens
		}
		t := genToken(store.ApiToken{
			ID: row.ID, UserID: row.UserID, OrgID: row.OrgID, Name: row.Name, Prefix: row.Prefix, Scopes: row.Scopes,
			ProjectIds: row.ProjectIds, ExpiresAt: row.ExpiresAt, LastUsedAt: row.LastUsedAt, LastUsedIp: row.LastUsedIp,
			RevokedAt: row.RevokedAt, CreatedVia: row.CreatedVia, CreatedAt: row.CreatedAt,
		}, now)
		t.OrgName = &row.OrgName
		t.OauthClientName = row.OauthClientName
		out.Items = append(out.Items, t)
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateToken implements POST /api/v1/tokens.
func (s *Server) CreateToken(w http.ResponseWriter, r *http.Request) {
	var req gen.CreateTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	scopes, err := tokens.NormalizeScopes(scopeStrings(req.Scopes))
	if err != nil {
		tokenError(s, w, "create token", err)
		return
	}
	exp := expiryDays(req.ExpiresInDays)
	if exp == 0 {
		exp = tokens.DefaultExpiry
	}
	if !s.checkTokenGrant(w, r, req.OrgId, scopes, req.ProjectIds, exp) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	p := tokens.CreateParams{UserID: sess.UserID, OrgID: req.OrgId, Name: req.Name, Scopes: scopes, ExpiresIn: expiryDays(req.ExpiresInDays), Via: "ui"}
	if sess.Token != nil {
		p.Via = "api"
	}
	if req.ProjectIds != nil {
		p.Projects = *req.ProjectIds
	}
	c, err := s.tokens.Create(r.Context(), p)
	if err != nil {
		tokenError(s, w, "create token", err)
		return
	}
	a := auditFrom(r.Context())
	a.orgID = req.OrgId
	a.target("token", c.Row.ID.String())
	a.set("name", c.Row.Name)
	a.set("scopes", c.Row.Scopes)
	writeJSON(w, http.StatusCreated, gen.CreatedToken{Secret: c.Secret, Token: genToken(c.Row, s.now())})
}

// RevokeMyToken implements DELETE /api/v1/tokens/{token_id}.
func (s *Server) RevokeMyToken(w http.ResponseWriter, r *http.Request, id gen.TokenID) {
	sess, _ := sessionFrom(r.Context())
	if sess.Token != nil {
		t, err := store.New(s.db).GetUserToken(r.Context(), store.GetUserTokenParams{ID: id, UserID: sess.UserID})
		if err == nil && t.OrgID != sess.Token.OrgID {
			writeError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
	}
	if err := s.tokens.RevokeOwn(r.Context(), sess.UserID, id); err != nil {
		tokenError(s, w, "revoke token", err)
		return
	}
	auditFrom(r.Context()).target("token", id.String())
	w.WriteHeader(http.StatusNoContent)
}

// ListOrgTokens implements GET /api/v1/orgs/{org}/tokens.
func (s *Server) ListOrgTokens(w http.ResponseWriter, r *http.Request, org gen.OrgID) {
	rows, err := store.New(s.db).ListOrgTokens(r.Context(), org)
	if err != nil {
		s.internalError(w, "list tokens", err)
		return
	}
	now := s.now()
	out := gen.APITokenList{Items: []gen.APIToken{}}
	for _, row := range rows {
		t := genToken(store.ApiToken{
			ID: row.ID, UserID: row.UserID, OrgID: row.OrgID, Name: row.Name, Prefix: row.Prefix, Scopes: row.Scopes,
			ProjectIds: row.ProjectIds, ExpiresAt: row.ExpiresAt, LastUsedAt: row.LastUsedAt, LastUsedIp: row.LastUsedIp,
			RevokedAt: row.RevokedAt, CreatedVia: row.CreatedVia, CreatedAt: row.CreatedAt,
		}, now)
		t.UserEmail = &row.UserEmail
		t.OauthClientName = row.OauthClientName
		out.Items = append(out.Items, t)
	}
	writeJSON(w, http.StatusOK, out)
}

// RevokeOrgToken implements DELETE /api/v1/orgs/{org}/tokens/{token_id}.
func (s *Server) RevokeOrgToken(w http.ResponseWriter, r *http.Request, org gen.OrgID, id gen.TokenID) {
	sess, _ := sessionFrom(r.Context())
	if err := s.tokens.RevokeInOrg(r.Context(), org, id, sess.UserID); err != nil {
		tokenError(s, w, "revoke token", err)
		return
	}
	auditFrom(r.Context()).target("token", id.String())
	w.WriteHeader(http.StatusNoContent)
}

// ---- Device login ------------------------------------------------------------

// StartDeviceLogin implements POST /api/v1/auth/device.
func (s *Server) StartDeviceLogin(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		writeError(w, http.StatusNotFound, "not_found", "API tokens are not enabled")
		return
	}
	if err := s.auth.AllowToken(ipString(r.Context())); err != nil {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts; wait a few minutes")
		return
	}
	var req gen.DeviceStartRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var scopes []string
	if req.Scopes != nil {
		scopes = scopeStrings(*req.Scopes)
	}
	name := ""
	if req.ClientName != nil {
		name = *req.ClientName
	}
	d, err := s.tokens.StartDevice(r.Context(), name, scopes)
	if err != nil {
		tokenError(s, w, "start device login", err)
		return
	}
	auditFrom(r.Context()).skip = true
	base := s.publicURL(r)
	writeJSON(w, http.StatusOK, gen.DeviceAuthorization{
		DeviceCode: d.DeviceCode, UserCode: d.UserCode,
		VerificationUri: base + "/device", VerificationUriComplete: base + "/device?code=" + d.UserCode,
		ExpiresIn: int(time.Until(d.ExpiresAt).Round(time.Second).Seconds()), Interval: int(d.Interval.Seconds()),
	})
}

// PollDeviceLogin implements POST /api/v1/auth/device/token.
func (s *Server) PollDeviceLogin(w http.ResponseWriter, r *http.Request) {
	if s.tokens == nil {
		writeError(w, http.StatusNotFound, "not_found", "API tokens are not enabled")
		return
	}
	var req gen.DevicePollRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	a.skip = true
	t, err := s.tokens.PollDevice(r.Context(), req.DeviceCode)
	switch {
	case errors.Is(err, tokens.ErrPending):
		writeError(w, http.StatusBadRequest, "authorization_pending", "waiting for you to approve the login in the browser")
		return
	case errors.Is(err, tokens.ErrSlowDown):
		writeError(w, http.StatusBadRequest, "slow_down", "polling too often")
		return
	case errors.Is(err, tokens.ErrDenied):
		writeError(w, http.StatusBadRequest, "access_denied", "the login was denied")
		return
	case errors.Is(err, tokens.ErrExpired):
		writeError(w, http.StatusBadRequest, "expired_token", "the login code expired; start again")
		return
	case err != nil:
		s.internalError(w, "poll device login", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.CreatedToken{Secret: t.Secret, Token: genToken(t.Row, s.now())})
}

// GetDeviceLogin implements GET /api/v1/auth/device/requests/{user_code}.
func (s *Server) GetDeviceLogin(w http.ResponseWriter, r *http.Request, userCode string) {
	d, err := s.tokens.LookupDevice(r.Context(), userCode)
	if err != nil {
		if errors.Is(err, tokens.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no pending login with that code; it may have expired")
			return
		}
		s.internalError(w, "device login", err)
		return
	}
	out := gen.DeviceRequest{UserCode: d.UserCode, ClientName: d.ClientName, ExpiresAt: d.ExpiresAt}
	for _, sc := range d.Scopes {
		out.Scopes = append(out.Scopes, gen.TokenScope(sc))
	}
	writeJSON(w, http.StatusOK, out)
}

// ApproveDeviceLogin implements POST /api/v1/auth/device/approve.
func (s *Server) ApproveDeviceLogin(w http.ResponseWriter, r *http.Request) {
	var req gen.DeviceApproveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	a := auditFrom(r.Context())
	if req.Approve != nil && !*req.Approve {
		if err := s.tokens.DenyDevice(r.Context(), req.UserCode); err != nil {
			tokenError(s, w, "deny device login", err)
			return
		}
		a.set("denied", true)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Approving issues a token, as creating one does: a session needs a
	// recent step-up, so a link sent to a signed-in user can't be one click.
	if sess, _ := sessionFrom(r.Context()); sess.Token == nil && !s.auth.RecentlyReauthenticated(sess) {
		writeError(w, http.StatusForbidden, "reauth_required", "confirm your password and code to continue")
		return
	}
	if req.OrgId == nil {
		writeError(w, http.StatusBadRequest, "invalid", "choose the organisation the CLI will act in")
		return
	}
	d, err := s.tokens.LookupDevice(r.Context(), req.UserCode)
	if err != nil {
		tokenError(s, w, "device login", err)
		return
	}
	scopes := d.Scopes
	if req.Scopes != nil {
		scopes = scopeStrings(*req.Scopes)
	}
	scopes, err = tokens.NormalizeScopes(scopes)
	if err != nil {
		tokenError(s, w, "approve device login", err)
		return
	}
	exp := expiryDays(req.ExpiresInDays)
	if !s.checkTokenGrant(w, r, *req.OrgId, scopes, req.ProjectIds, max(exp, tokens.DefaultExpiry)) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	p := tokens.CreateParams{UserID: sess.UserID, OrgID: *req.OrgId, Scopes: scopes, ExpiresIn: exp}
	if req.Name != nil {
		p.Name = *req.Name
	}
	if req.ProjectIds != nil {
		p.Projects = *req.ProjectIds
	}
	t, err := s.tokens.ApproveDevice(r.Context(), req.UserCode, p)
	if err != nil {
		tokenError(s, w, "approve device login", err)
		return
	}
	a.orgID = *req.OrgId
	a.target("token", t.ID.String())
	a.set("scopes", t.Scopes)
	writeJSON(w, http.StatusOK, genToken(t, s.now()))
}

// ---- Platform settings ---------------------------------------------------------

// GetTokenSettings implements GET /api/v1/admin/settings/tokens.
func (s *Server) GetTokenSettings(w http.ResponseWriter, r *http.Request) {
	st, err := s.tokens.GetSettings(r.Context())
	if err != nil {
		s.internalError(w, "token settings", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.TokenSettings{MaxDays: st.MaxDays})
}

// PutTokenSettings implements PUT /api/v1/admin/settings/tokens.
func (s *Server) PutTokenSettings(w http.ResponseWriter, r *http.Request) {
	var req gen.TokenSettings
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.tokens.PutSettings(r.Context(), tokens.Settings{MaxDays: req.MaxDays}); err != nil {
		tokenError(s, w, "token settings", err)
		return
	}
	auditFrom(r.Context()).set("max_days", req.MaxDays)
	writeJSON(w, http.StatusOK, req)
}
