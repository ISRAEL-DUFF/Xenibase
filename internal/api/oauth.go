package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tokens"
)

// OAuth 2.0 for third-party apps (RFC 6749 §4.1 with PKCE, RFC 7636): the
// consent page's two calls, the app's token and revocation endpoints, and
// the platform admin's client registry. A consent ends in an ordinary API
// token, so everything after the exchange is internal/api/tokens.go.

// oauthError writes an error in OAuth's shape (RFC 6749 §5.2).
func oauthError(w http.ResponseWriter, status int, code gen.OAuthErrorError, desc string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="Xenibase OAuth"`)
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	}
	writeJSON(w, status, gen.OAuthError{Error: code, ErrorDescription: desc})
}

// oauthParams reads a token or revocation request: form-encoded or JSON,
// with the client's credentials in the body or as HTTP Basic auth (RFC
// 6749 §2.3.1), but not both.
func oauthParams(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	v := url.Values{}
	body := http.MaxBytesReader(w, r.Body, 1<<16)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var m map[string]any
		if err := json.NewDecoder(body).Decode(&m); err != nil && !errors.Is(err, io.EOF) {
			oauthError(w, http.StatusBadRequest, gen.InvalidRequest, "invalid JSON body: "+err.Error())
			return nil, false
		}
		for k, val := range m {
			if s, ok := val.(string); ok {
				v.Set(k, s)
			}
		}
	} else {
		r.Body = body
		if err := r.ParseForm(); err != nil {
			oauthError(w, http.StatusBadRequest, gen.InvalidRequest, "invalid form body: "+err.Error())
			return nil, false
		}
		for k, vals := range r.PostForm {
			if len(vals) > 0 {
				v.Set(k, vals[0])
			}
		}
	}
	if id, secret, ok := r.BasicAuth(); ok {
		if (v.Get("client_id") != "" && v.Get("client_id") != id) || v.Get("client_secret") != "" {
			oauthError(w, http.StatusBadRequest, gen.InvalidRequest, "send the client credentials once: as HTTP Basic auth or in the body")
			return nil, false
		}
		v.Set("client_id", id)
		v.Set("client_secret", secret)
	}
	return v, true
}

// oauthRedirect appends parameters to the app's redirect URI, keeping its
// own query (RFC 6749 §4.1.2); empty values are left out.
func oauthRedirect(base string, kv ...string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			q.Set(kv[i], kv[i+1])
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) requireTokens(w http.ResponseWriter) bool {
	if s.tokens == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "API tokens are not enabled on this server")
		return false
	}
	return true
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// grantableOrgs lists the organisations the user may consent for, with
// the projects they could restrict the token to (what Account > New token
// offers).
func (s *Server) grantableOrgs(r *http.Request, userID uuid.UUID) ([]gen.OAuthGrantOrg, error) {
	ctx := r.Context()
	q := store.New(s.db)
	rows, err := q.ListUserOrgs(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]gen.OAuthGrantOrg, 0, len(rows))
	for _, o := range rows {
		if o.Status != "active" {
			continue
		}
		seeAll := o.MemberRole == authz.OrgOwner || o.MemberRole == authz.OrgAdmin
		ps, err := q.ListOrgProjects(ctx, store.ListOrgProjectsParams{OrgID: o.ID, SeeAll: seeAll, UserID: userID, MaxRows: 500})
		if err != nil {
			return nil, err
		}
		g := gen.OAuthGrantOrg{Id: o.ID, Name: o.Name, Personal: o.PersonalOwnerID != nil, Projects: make([]gen.OAuthGrantProject, 0, len(ps))}
		for _, p := range ps {
			g.Projects = append(g.Projects, gen.OAuthGrantProject{Id: p.ID, Name: p.Name})
		}
		out = append(out, g)
	}
	return out, nil
}

// GetOAuthAuthorization implements GET /api/v1/oauth/authorize.
func (s *Server) GetOAuthAuthorization(w http.ResponseWriter, r *http.Request, params gen.GetOAuthAuthorizationParams) {
	if !s.requireTokens(w) {
		return
	}
	req := tokens.AuthorizeRequest{
		ClientID: params.ClientId, RedirectURI: params.RedirectUri, Scope: deref(params.Scope), State: deref(params.State),
		CodeChallenge: params.CodeChallenge, CodeChallengeMethod: params.CodeChallengeMethod,
	}
	a, err := s.tokens.ValidateAuthorize(r.Context(), req)
	if err != nil {
		tokenError(s, w, "oauth authorize", err)
		return
	}
	sess, _ := sessionFrom(r.Context())
	orgs, err := s.grantableOrgs(r, sess.UserID)
	if err != nil {
		s.internalError(w, "oauth authorize", err)
		return
	}
	out := gen.OAuthAuthorizeInfo{
		ClientId: a.Client.ClientID, ClientName: a.Client.Name, RedirectUri: req.RedirectURI, State: params.State,
		Scopes: []gen.TokenScope{}, CodeChallenge: req.CodeChallenge, CodeChallengeMethod: req.CodeChallengeMethod, Organisations: orgs,
	}
	for _, sc := range a.Scopes {
		out.Scopes = append(out.Scopes, gen.TokenScope(sc))
	}
	writeJSON(w, http.StatusOK, out)
}

// DecideOAuthAuthorization implements POST /api/v1/oauth/authorize.
func (s *Server) DecideOAuthAuthorization(w http.ResponseWriter, r *http.Request) {
	if !s.requireTokens(w) {
		return
	}
	var req gen.OAuthDecision
	if !decodeJSON(w, r, &req) {
		return
	}
	areq := tokens.AuthorizeRequest{
		ClientID: req.ClientId, RedirectURI: req.RedirectUri, State: deref(req.State),
		CodeChallenge: req.CodeChallenge, CodeChallengeMethod: req.CodeChallengeMethod,
	}
	if req.Scopes != nil {
		areq.Scope = strings.Join(scopeStrings(*req.Scopes), " ")
	}
	a, err := s.tokens.ValidateAuthorize(r.Context(), areq)
	if err != nil {
		tokenError(s, w, "oauth authorize", err)
		return
	}
	audit := auditFrom(r.Context())
	audit.target("oauth_client", a.Client.ClientID)
	audit.set("client_name", a.Client.Name)
	if !req.Approve {
		audit.set("denied", true)
		writeJSON(w, http.StatusOK, gen.OAuthRedirect{RedirectTo: oauthRedirect(areq.RedirectURI,
			"error", "access_denied", "error_description", "the user denied the request", "state", areq.State)})
		return
	}
	if req.OrgId == nil {
		writeError(w, http.StatusBadRequest, "invalid", "choose the organisation the app will act in")
		return
	}
	scopes := a.Scopes
	exp := expiryDays(req.ExpiresInDays)
	if !s.checkTokenGrant(w, r, *req.OrgId, scopes, req.ProjectIds, max(exp, tokens.DefaultExpiry)) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	p := tokens.CodeParams{Request: areq, UserID: sess.UserID, OrgID: *req.OrgId, Scopes: scopes, ExpiresIn: exp}
	if req.ProjectIds != nil {
		p.Projects = *req.ProjectIds
	}
	code, err := s.tokens.IssueCode(r.Context(), p)
	if err != nil {
		tokenError(s, w, "oauth authorize", err)
		return
	}
	audit.orgID = *req.OrgId
	audit.set("scopes", scopes)
	if req.ProjectIds != nil {
		audit.set("project_ids", *req.ProjectIds)
	}
	writeJSON(w, http.StatusOK, gen.OAuthRedirect{RedirectTo: oauthRedirect(areq.RedirectURI, "code", code, "state", areq.State)})
}

// ExchangeOAuthToken implements POST /api/v1/oauth/token.
func (s *Server) ExchangeOAuthToken(w http.ResponseWriter, r *http.Request) {
	audit := auditFrom(r.Context())
	audit.skip = true // only an issued (or revoked) token is an event
	if s.tokens == nil {
		oauthError(w, http.StatusServiceUnavailable, gen.ServerError, "API tokens are not enabled on this server")
		return
	}
	if err := s.auth.AllowToken(ipString(r.Context())); err != nil {
		oauthError(w, http.StatusTooManyRequests, gen.SlowDown, "too many attempts from this address; wait a few minutes")
		return
	}
	v, ok := oauthParams(w, r)
	if !ok {
		return
	}
	if v.Get("grant_type") != string(gen.AuthorizationCode) {
		oauthError(w, http.StatusBadRequest, gen.UnsupportedGrantType, "grant_type must be authorization_code")
		return
	}
	c, err := s.tokens.Exchange(r.Context(), tokens.ExchangeParams{
		Code: v.Get("code"), RedirectURI: v.Get("redirect_uri"), ClientID: v.Get("client_id"),
		ClientSecret: v.Get("client_secret"), CodeVerifier: v.Get("code_verifier"),
	})
	audit.target("oauth_client", v.Get("client_id"))
	switch {
	case errors.Is(err, tokens.ErrInvalidClient):
		oauthError(w, http.StatusUnauthorized, gen.InvalidClient, strings.TrimPrefix(err.Error(), tokens.ErrInvalidClient.Error()+": "))
		return
	case errors.Is(err, tokens.ErrInvalidGrant):
		if strings.Contains(err.Error(), "revoked") {
			audit.skip = false
			audit.set("reason", "code_reuse")
		}
		oauthError(w, http.StatusBadRequest, gen.InvalidGrant, strings.TrimPrefix(err.Error(), tokens.ErrInvalidGrant.Error()+": "))
		return
	case errors.Is(err, tokens.ErrInvalidRequest):
		oauthError(w, http.StatusBadRequest, gen.InvalidRequest, strings.TrimPrefix(err.Error(), tokens.ErrInvalidRequest.Error()+": "))
		return
	case errors.Is(err, tokens.ErrInvalid):
		oauthError(w, http.StatusBadRequest, gen.InvalidRequest, strings.TrimPrefix(err.Error(), tokens.ErrInvalid.Error()+": "))
		return
	case err != nil:
		s.log.Error("oauth token exchange", "err", err)
		oauthError(w, http.StatusInternalServerError, gen.ServerError, "internal error")
		return
	}
	audit.skip = false
	audit.userID = &c.Row.UserID
	audit.orgID = c.Row.OrgID
	audit.set("token_id", c.Row.ID)
	audit.set("scopes", c.Row.Scopes)
	now := s.now()
	out := gen.OAuthTokenResponse{
		AccessToken: c.Secret, TokenType: gen.Bearer, ExpiresIn: int(c.Row.ExpiresAt.Sub(now).Seconds()),
		Scope: strings.Join(c.Row.Scopes, " "), OrgId: c.Row.OrgID, TokenId: c.Row.ID,
	}
	if c.Row.ProjectIds != nil {
		ids := append([]uuid.UUID{}, c.Row.ProjectIds...)
		out.ProjectIds = &ids
	}
	w.Header().Set("Pragma", "no-cache") // RFC 6749 §5.1
	writeJSON(w, http.StatusOK, out)
}

// RevokeOAuthToken implements POST /api/v1/oauth/revoke (RFC 7009).
func (s *Server) RevokeOAuthToken(w http.ResponseWriter, r *http.Request) {
	audit := auditFrom(r.Context())
	audit.skip = true
	if s.tokens == nil {
		oauthError(w, http.StatusServiceUnavailable, gen.ServerError, "API tokens are not enabled on this server")
		return
	}
	if err := s.auth.AllowToken(ipString(r.Context())); err != nil {
		oauthError(w, http.StatusTooManyRequests, gen.SlowDown, "too many attempts from this address; wait a few minutes")
		return
	}
	v, ok := oauthParams(w, r)
	if !ok {
		return
	}
	if v.Get("token") == "" {
		oauthError(w, http.StatusBadRequest, gen.InvalidRequest, "token is required")
		return
	}
	id, revoked, err := s.tokens.RevokeByClient(r.Context(), v.Get("client_id"), v.Get("client_secret"), v.Get("token"))
	switch {
	case errors.Is(err, tokens.ErrInvalidClient):
		oauthError(w, http.StatusUnauthorized, gen.InvalidClient, strings.TrimPrefix(err.Error(), tokens.ErrInvalidClient.Error()+": "))
		return
	case err != nil:
		s.log.Error("oauth token revoke", "err", err)
		oauthError(w, http.StatusInternalServerError, gen.ServerError, "internal error")
		return
	}
	if revoked {
		audit.skip = false
		audit.target("token", id.String())
		audit.set("client_id", v.Get("client_id"))
	}
	w.WriteHeader(http.StatusOK)
}

// ---- Platform admin: the client registry -----------------------------------------

func genOAuthClient(c store.OauthClient) gen.OAuthClient {
	out := gen.OAuthClient{
		Id: c.ID, ClientId: c.ClientID, Name: c.Name, Confidential: c.ClientSecretHash != nil,
		RedirectUris: c.RedirectUris, ScopesAllowed: []gen.TokenScope{}, CreatedAt: c.CreatedAt, DisabledAt: c.DisabledAt,
	}
	for _, sc := range c.ScopesAllowed {
		out.ScopesAllowed = append(out.ScopesAllowed, gen.TokenScope(sc))
	}
	return out
}

// ListOAuthClients implements GET /api/v1/admin/oauth/clients.
func (s *Server) ListOAuthClients(w http.ResponseWriter, r *http.Request) {
	if !s.requireTokens(w) {
		return
	}
	rows, err := s.tokens.ListClients(r.Context())
	if err != nil {
		s.internalError(w, "list oauth clients", err)
		return
	}
	out := gen.OAuthClientList{Items: make([]gen.OAuthClient, 0, len(rows))}
	for _, c := range rows {
		out.Items = append(out.Items, genOAuthClient(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateOAuthClient implements POST /api/v1/admin/oauth/clients.
func (s *Server) CreateOAuthClient(w http.ResponseWriter, r *http.Request) {
	if !s.requireTokens(w) {
		return
	}
	var req gen.CreateOAuthClientRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	p := tokens.ClientParams{Name: req.Name, RedirectURIs: req.RedirectUris, Confidential: req.Confidential == nil || *req.Confidential, CreatedBy: userID(r.Context())}
	if req.ScopesAllowed != nil {
		p.Scopes = scopeStrings(*req.ScopesAllowed)
	}
	c, err := s.tokens.CreateClient(r.Context(), p)
	if err != nil {
		tokenError(s, w, "create oauth client", err)
		return
	}
	a := auditFrom(r.Context())
	a.target("oauth_client", c.Row.ClientID)
	a.set("name", c.Row.Name)
	a.set("redirect_uris", c.Row.RedirectUris)
	a.set("scopes_allowed", c.Row.ScopesAllowed)
	out := gen.OAuthClientCreated{Client: genOAuthClient(c.Row)}
	if c.Secret != "" {
		out.ClientSecret = &c.Secret
	}
	writeJSON(w, http.StatusCreated, out)
}

// DisableOAuthClient implements DELETE /api/v1/admin/oauth/clients/{client_id}.
func (s *Server) DisableOAuthClient(w http.ResponseWriter, r *http.Request, clientID string) {
	if !s.requireTokens(w) {
		return
	}
	sess, _ := sessionFrom(r.Context())
	if err := s.tokens.DisableClient(r.Context(), clientID, sess.UserID); err != nil {
		tokenError(s, w, "disable oauth client", err)
		return
	}
	auditFrom(r.Context()).target("oauth_client", clientID)
	w.WriteHeader(http.StatusNoContent)
}
