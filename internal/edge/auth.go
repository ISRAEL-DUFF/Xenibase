package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/jwtes"
	"github.com/israel-duff/xenibase/internal/messaging"
	"github.com/israel-duff/xenibase/internal/projauth"
)

// Auth (V4 §4): sign-up, sign-in, sessions and tokens at /auth/v1/. It runs
// in the project's database as the edge login, which alone can read the
// xb_auth tables; tokens are signed with the project's active key.

const (
	maxAuthBody = 64 << 10
	// emailEvery is how often one address may be sent a code.
	emailEvery = 60 * time.Second
	maxPassLen = 256
)

// Per-IP limits on auth endpoints, per minute, on top of the project's.
var authRates = map[string]int{"signup": 10, "signin": 30, "otp": 10, "verify": 30, "token": 120, "recover": 10, "admin": 600}

// authBody reads a JSON body into v (an empty body is {}).
func authBody(c *call, v any) bool {
	b, err := io.ReadAll(io.LimitReader(c.r.Body, maxAuthBody+1))
	if err != nil || len(b) > maxAuthBody {
		c.fail(http.StatusRequestEntityTooLarge, "body_too_large", "the request body is too large")
		return false
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		b = []byte("{}")
	}
	if err := json.Unmarshal(b, v); err != nil {
		c.fail(http.StatusBadRequest, "invalid_body", "the body must be a JSON object: "+err.Error())
		return false
	}
	return true
}

// withAuth runs fn in one transaction as the edge login itself (no request
// role): only it may read and write xb_auth.
func (e *Edge) withAuth(ctx context.Context, p *project, fn func(pgx.Tx) error) error {
	pool, err := e.dbPool(ctx, p)
	if err != nil {
		return err
	}
	// A connection the pooler dropped (a reload, a restart) fails before
	// anything ran: that is retried once on a fresh connection.
	for attempt := 0; ; attempt++ {
		ran := false
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', '10000', true)`); err != nil {
				return err
			}
			ran = true
			return fn(tx)
		})
		if err == nil || ran || attempt > 0 || !connectionLost(err) {
			return err
		}
	}
}

// connectionLost reports an error from a connection that broke or was
// refused, not from SQL.
func connectionLost(err error) bool {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return unavailable(pe.Code)
	}
	return pgconn.SafeToRetry(err) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// hash runs a password hash within the edge's hashing slots, so a burst of
// sign-ins can't take every CPU.
func (e *Edge) hash(f func()) {
	e.hashSlots <- struct{}{}
	defer func() { <-e.hashSlots }()
	f()
}

// keylessAuth serves what works without an API key: the public keys, and
// the links in emails. false means the request goes on to the key check.
func (e *Edge) keylessAuth(c *call) bool {
	switch {
	case c.r.URL.Path == "/auth/v1/.well-known/jwks.json" && c.r.Method == http.MethodGet:
		e.jwks(c)
		return true
	case c.r.URL.Path == "/auth/v1/verify" && c.r.Method == http.MethodGet:
		if !e.authLimit(c, "verify") {
			return true
		}
		e.verifyLink(c)
		return true
	case c.r.URL.Path == "/auth/v1/authorize" && c.r.Method == http.MethodGet:
		e.oauthAuthorize(c, nil)
		return true
	case c.r.URL.Path == "/auth/v1/callback" && (c.r.Method == http.MethodGet || c.r.Method == http.MethodPost):
		e.callback(c)
		return true
	}
	return false
}

func (e *Edge) authLimit(c *call, group string) bool {
	if !e.limits.allow("auth:"+group+":"+c.p.cfg.Ref+":"+c.ip, authRates[group]*max(1, e.cfg.AuthRateScale)) {
		c.w.Header().Set("Retry-After", "10")
		c.fail(http.StatusTooManyRequests, "over_request_rate_limit", "too many requests from this address; slow down")
		return false
	}
	return true
}

func (e *Edge) jwks(c *call) {
	keys := make([]json.RawMessage, 0, len(c.p.cfg.JWKs))
	keys = append(keys, c.p.cfg.JWKs...)
	c.w.Header().Set("Cache-Control", "public, max-age=600")
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(c.w).Encode(map[string]any{"keys": keys})
}

// auth routes /auth/v1/... after the key check.
func (e *Edge) auth(c *call, req Request) {
	path := strings.TrimPrefix(c.r.URL.Path, "/auth/v1/")
	m := c.r.Method
	switch {
	case path == ".well-known/jwks.json" && m == http.MethodGet:
		e.jwks(c)
	case path == "settings" && m == http.MethodGet:
		a := c.p.cfg.Auth
		external := map[string]bool{"email": true, "phone": len(a.PhoneChannels) > 0, "anonymous_users": a.AnonymousEnabled}
		for name := range a.OAuth {
			external[name] = true
		}
		c.json(http.StatusOK, map[string]any{"signup_enabled": a.SignupEnabled, "email_confirm": a.EmailConfirm,
			"magic_link_enabled": a.MagicLinkEnabled, "password_min_length": a.PasswordMinLength,
			"password_require_mixed": a.PasswordRequireMixed, "external": external, "phone_channels": a.PhoneChannels,
			"phone_confirm": a.PhoneConfirm, "mfa": a.MFA, "mfa_phone": a.MFAPhone, "captcha": a.CaptchaSecret != ""})
	case path == "signup" && m == http.MethodPost:
		if e.authLimit(c, "signup") {
			e.signup(c)
		}
	case path == "signin/password" && m == http.MethodPost:
		if e.authLimit(c, "signin") {
			e.signinPassword(c)
		}
	case path == "token" && m == http.MethodPost:
		switch c.r.URL.Query().Get("grant_type") {
		case "refresh_token":
			if e.authLimit(c, "token") {
				e.refresh(c)
			}
		case "password":
			if e.authLimit(c, "signin") {
				e.signinPassword(c)
			}
		case "pkce":
			if e.authLimit(c, "token") {
				e.exchangePKCE(c)
			}
		default:
			c.fail(http.StatusBadRequest, "unsupported_grant_type", "grant_type is refresh_token, password or pkce")
		}
	case path == "signin/otp" && m == http.MethodPost:
		if e.authLimit(c, "otp") {
			e.signinOTP(c)
		}
	case path == "verify" && m == http.MethodPost:
		if e.authLimit(c, "verify") {
			e.verify(c)
		}
	case path == "resend" && m == http.MethodPost:
		if e.authLimit(c, "otp") {
			e.resend(c)
		}
	case path == "recover" && m == http.MethodPost:
		if e.authLimit(c, "recover") {
			e.recover(c)
		}
	case path == "signout" && m == http.MethodPost:
		e.signout(c, req)
	case path == "user" && m == http.MethodGet:
		e.getUser(c, req)
	case path == "user" && (m == http.MethodPatch || m == http.MethodPut):
		e.updateUser(c, req)
	case strings.HasPrefix(path, "user/identities/") && m == http.MethodDelete:
		e.unlinkIdentity(c, req, strings.TrimPrefix(path, "user/identities/"))
	case path == "user/identities/authorize" && m == http.MethodGet:
		e.oauthAuthorize(c, &req)
	case strings.HasPrefix(path, "factors") || strings.HasPrefix(path, "mfa/"):
		e.mfa(c, req, path)
	case strings.HasPrefix(path, "admin/"):
		if req.Role != "service" {
			c.fail(http.StatusForbidden, "admin_only", "the admin API needs the project's secret key")
			return
		}
		if e.authLimit(c, "admin") {
			e.admin(c, strings.TrimPrefix(path, "admin/"))
		}
	default:
		c.fail(http.StatusNotFound, "no_such_endpoint", "no such auth endpoint")
	}
}

// apiErr is a refusal decided inside a transaction that still commits (a
// failed attempt counted, a reused token's session ended).
type apiErr struct {
	status     int
	code, msg  string
	retryAfter int
}

func (a *apiErr) Error() string { return a.code + ": " + a.msg }

func (a *apiErr) send(c *call) {
	if a.retryAfter > 0 {
		c.w.Header().Set("Retry-After", strconv.Itoa(a.retryAfter))
	}
	c.fail(a.status, a.code, a.msg)
}

func refuse(status int, code, msg string) *apiErr {
	return &apiErr{status: status, code: code, msg: msg}
}

var (
	errNoSigningKey = errors.New("the project has no signing key yet")
	errEmailRate    = errors.New("email rate limit")
)

// authDBError answers an error from an auth transaction.
func (e *Edge) authDBError(c *call, err error) {
	var a *apiErr
	switch {
	case errors.As(err, &a):
		a.send(c)
	case errors.Is(err, errEmailRate):
		c.w.Header().Set("Retry-After", "3600")
		c.fail(http.StatusTooManyRequests, "over_email_send_rate_limit",
			"the project's email limit is used up (the platform's email is for development; set up the project's own SMTP)")
	case errors.Is(err, errNoSigningKey):
		c.w.Header().Set("Retry-After", "5")
		c.fail(http.StatusServiceUnavailable, "signing_key_unavailable", "the project's signing key isn't ready; retry shortly")
	case errors.Is(err, projauth.ErrExists):
		c.fail(http.StatusUnprocessableEntity, "user_already_exists", "a user with this email address already exists")
	case errors.Is(err, projauth.ErrNotFound):
		c.fail(http.StatusNotFound, "user_not_found", "no such user")
	default:
		e.dbError(c, err)
	}
}

// ---- Rules -------------------------------------------------------------------

// emailRe is a plain address (dot-atom local part, a dotted domain): no
// quoting, commas or brackets that could reach an email header.
var emailRe = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)+$`)

func validEmail(s string) bool { return len(s) <= 254 && emailRe.MatchString(s) }

func passwordProblem(a edgeapi.AuthConfig, pw string) string {
	n := utf8.RuneCountInString(pw)
	if n < a.PasswordMinLength {
		return fmt.Sprintf("the password must be at least %d characters", a.PasswordMinLength)
	}
	if len(pw) > maxPassLen {
		return fmt.Sprintf("the password must be at most %d bytes", maxPassLen)
	}
	if a.PasswordRequireMixed {
		var letter, digit bool
		for _, r := range pw {
			letter = letter || unicode.IsLetter(r)
			digit = digit || unicode.IsDigit(r)
		}
		if !letter || !digit {
			return "the password must have both letters and digits"
		}
	}
	return ""
}

// redirectFor is where a link may go: redirect_to if allowed, else the
// site URL ("" if neither).
func redirectFor(a edgeapi.AuthConfig, want string) (string, bool) {
	if want == "" {
		return a.SiteURL, true
	}
	if redirectAllowed(a, want) {
		return want, true
	}
	return "", false
}

// redirectAllowed reports whether u is somewhere links may go: under the
// site URL, an exact redirect URL, or (with wildcards on) one matching a
// redirect pattern. URLs with a user part are refused.
func redirectAllowed(a edgeapi.AuthConfig, u string) bool {
	pu, err := url.Parse(u)
	if err != nil || pu.Host == "" || pu.User != nil || pu.Opaque != "" {
		return false
	}
	if a.SiteURL != "" {
		if site, err := url.Parse(a.SiteURL); err == nil && strings.EqualFold(site.Scheme, pu.Scheme) &&
			strings.EqualFold(site.Host, pu.Host) && pathUnder(pu.Path, site.Path) {
			return true
		}
	}
	for _, r := range a.RedirectURLs {
		if r == u {
			return true
		}
		if a.AllowWildcardRedirects && strings.Contains(r, "*") && patternAllows(r, pu) {
			return true
		}
	}
	return false
}

// pathUnder reports whether p is base or below it.
func pathUnder(p, base string) bool {
	base = strings.TrimSuffix(base, "/")
	return base == "" || p == base || strings.HasPrefix(p, base+"/")
}

// patternAllows matches a redirect pattern against a parsed URL part by
// part, never as one string: the scheme exactly; the host label by label,
// "*" matching within one label only, and the port exactly ("*" any
// port); the path with globMatch. A query or fragment is allowed only
// when the pattern has a matching one or its path ends in "**".
func patternAllows(pattern string, u *url.URL) bool {
	scheme, rest, ok := strings.Cut(pattern, "://")
	if !ok || !strings.EqualFold(scheme, u.Scheme) {
		return false
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	authority, rest := rest[:end], rest[end:]
	if strings.Contains(authority, "@") {
		return false
	}
	hostPat, portPat := authority, ""
	if i := strings.LastIndex(authority, ":"); i >= 0 && !strings.HasSuffix(authority, "]") {
		hostPat, portPat = authority[:i], authority[i+1:]
	}
	if portPat != "*" && portPat != u.Port() {
		return false
	}
	if !hostMatch(strings.Trim(hostPat, "[]"), u.Hostname()) {
		return false
	}
	pathPat, query, fragment := rest, "", ""
	hasQuery, hasFragment := false, false
	if i := strings.Index(pathPat, "#"); i >= 0 {
		pathPat, fragment, hasFragment = pathPat[:i], pathPat[i+1:], true
	}
	if i := strings.Index(pathPat, "?"); i >= 0 {
		pathPat, query, hasQuery = pathPat[:i], pathPat[i+1:], true
	}
	if pathPat == "" {
		pathPat = "/"
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if !globMatch(pathPat, path) {
		return false
	}
	trailing := strings.HasSuffix(pathPat, "**")
	if hasQuery {
		if !globMatch(query, u.RawQuery) {
			return false
		}
	} else if (u.RawQuery != "" || u.ForceQuery) && !trailing {
		return false
	}
	if hasFragment {
		return globMatch(fragment, u.EscapedFragment())
	}
	return u.Fragment == "" || trailing
}

// hostMatch matches a host pattern against a host name, ignoring case:
// the same number of labels, each label of the pattern matching the
// host's with "*" as any characters within it.
func hostMatch(pattern, host string) bool {
	ps, hs := strings.Split(strings.ToLower(pattern), "."), strings.Split(strings.ToLower(host), ".")
	if len(ps) != len(hs) {
		return false
	}
	for i := range ps {
		if hs[i] == "" || !globMatch(strings.ReplaceAll(ps[i], "**", "*"), hs[i]) {
			return false
		}
	}
	return true
}

// globMatch: "**" matches anything, "*" anything but "/".
func globMatch(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch {
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case pattern[i] == '*':
			b.WriteString("[^/]*")
		default:
			b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(s)
}

// ---- Tokens -------------------------------------------------------------------

// tokenResponse is a signed-in session (the shape Supabase's clients read).
type tokenResponse struct {
	AccessToken  string         `json:"access_token"`
	TokenType    string         `json:"token_type"`
	ExpiresIn    int            `json:"expires_in"`
	ExpiresAt    int64          `json:"expires_at"`
	RefreshToken string         `json:"refresh_token"`
	User         *projauth.User `json:"user"`
}

// issue signs an access token for u in session s.
func (e *Edge) issue(ctx context.Context, p *project, u *projauth.User, s *projauth.Session, refresh string) (tokenResponse, error) {
	if p.cfg.SigningKey == nil {
		return tokenResponse{}, errNoSigningKey
	}
	ttl := p.cfg.Auth.AccessTokenTTL
	if ttl <= 0 {
		ttl = 3600
	}
	now := time.Now()
	exp := now.Add(time.Duration(ttl) * time.Second)
	claims := map[string]any{
		"sub": u.ID.String(), "role": "user", "aud": p.cfg.Ref, "iat": now.Unix(), "exp": exp.Unix(),
		"session_id": s.ID.String(), "aal": s.AAL, "amr": s.AMR, "is_anonymous": u.IsAnonymous,
		"app_metadata": u.AppMetadata, "user_metadata": u.UserMetadata,
	}
	if u.Email != nil {
		claims["email"] = *u.Email
	}
	if u.Phone != nil {
		claims["phone"] = *u.Phone
	}
	claims, err := e.customClaims(ctx, p, u, s.LastMethod(), claims)
	if err != nil {
		return tokenResponse{}, err
	}
	tok, err := jwtes.Sign(p.cfg.SigningKey.Private, p.cfg.SigningKey.Kid, claims)
	if err != nil {
		return tokenResponse{}, err
	}
	return tokenResponse{AccessToken: tok, TokenType: "bearer", ExpiresIn: ttl, ExpiresAt: exp.Unix(), RefreshToken: refresh, User: u}, nil
}

// startSession signs u in: a new session (others ended with the
// single-session setting), the sign-in recorded, tokens issued.
func (e *Edge) startSession(ctx context.Context, c *call, tx pgx.Tx, u *projauth.User, method string) (tokenResponse, error) {
	a := c.p.cfg.Auth
	if err := e.mauAllowed(c, u.ID); err != nil {
		return tokenResponse{}, err
	}
	if a.SingleSession {
		if _, err := projauth.EndSessions(ctx, tx, u.ID, nil); err != nil {
			return tokenResponse{}, err
		}
	}
	if err := projauth.RecordSignIn(ctx, tx, u.ID); err != nil {
		return tokenResponse{}, err
	}
	s, refresh, err := projauth.NewSession(ctx, tx, u.ID, method, c.r.UserAgent(), c.ip, time.Duration(a.SessionMaxSeconds)*time.Second)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := projauth.Audit(ctx, tx, &u.ID, projauth.ActSignIn, c.ip, map[string]any{"method": method}); err != nil {
		return tokenResponse{}, err
	}
	fresh, err := projauth.GetUser(ctx, tx, u.ID, false)
	if err != nil {
		return tokenResponse{}, err
	}
	e.meter.activeUser(c.p.cfg.ProjectID, u.ID)
	c.userID = &u.ID
	if a.AfterSigninHook {
		c.hooks = append(c.hooks, edgeapi.AuthHook{Ref: c.p.cfg.Ref, Event: "after_signin",
			Payload: map[string]any{"user": fresh, "method": method, "session_id": s.ID}})
	}
	return e.issue(ctx, c.p, fresh, s, refresh)
}

// ---- Email --------------------------------------------------------------------

// verifyLink is the one-click link for a code's token.
func (e *Edge) verifyLink(c *call) {
	q := c.r.URL.Query()
	typ, token := q.Get("type"), q.Get("token")
	to, ok := redirectFor(c.p.cfg.Auth, q.Get("redirect_to"))
	if !ok {
		linkPage(c, http.StatusBadRequest, "This link's redirect address isn't allowed for this project.")
		return
	}
	kinds := verifyKinds(typ)
	if kinds == nil || token == "" {
		linkPage(c, http.StatusBadRequest, "This link is malformed.")
		return
	}
	var out tokenResponse
	var fail *apiErr
	err := e.authTx(c, func(tx pgx.Tx) error {
		v, err := projauth.VerifyToken(c.r.Context(), tx, token, kinds)
		if err != nil {
			return err
		}
		if !v.OK {
			fail = refuse(http.StatusForbidden, "otp_expired", "the link is invalid or has expired")
			return nil
		}
		out, fail, err = e.useVerified(c.r.Context(), c, tx, v)
		return err
	})
	if err != nil {
		e.cfg.Log.Warn("auth verify link", "project", c.p.cfg.Ref, "err", err)
		fail = refuse(http.StatusServiceUnavailable, "server_error", "the link couldn't be checked; try again")
	}
	if to == "" {
		if fail != nil {
			linkPage(c, fail.status, "This link is invalid or has expired.")
		} else {
			linkPage(c, http.StatusOK, "Your email is confirmed. You can close this page and sign in.")
		}
		return
	}
	frag := url.Values{}
	if fail != nil {
		frag.Set("error", "access_denied")
		frag.Set("error_code", fail.code)
		frag.Set("error_description", fail.msg)
	} else {
		frag.Set("access_token", out.AccessToken)
		frag.Set("refresh_token", out.RefreshToken)
		frag.Set("expires_in", strconv.Itoa(out.ExpiresIn))
		frag.Set("expires_at", strconv.FormatInt(out.ExpiresAt, 10))
		frag.Set("token_type", "bearer")
		frag.Set("type", typ)
	}
	u, _ := url.Parse(to)
	u.Fragment = ""
	c.w.Header().Set("Cache-Control", "no-store")
	c.w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(c.w, c.r, u.String()+"#"+frag.Encode(), http.StatusSeeOther)
}

func linkPage(c *call, status int, msg string) {
	h := c.w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	c.w.WriteHeader(status)
	_, _ = fmt.Fprintf(c.w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">`+
		`<title>Sign-in</title><p style="font:16px system-ui;max-width:32rem;margin:4rem auto;padding:0 1rem">%s</p>`, html.EscapeString(msg))
}

// sendEmail asks xenibase-server to send an auth email with code and link.
func (e *Edge) sendEmail(ctx context.Context, p *project, kind, linkType, to string, code projauth.Code, redirect string) error {
	q := url.Values{"token": {code.Token}, "type": {linkType}}
	if redirect != "" {
		q.Set("redirect_to", redirect)
	}
	link := "https://" + p.cfg.Ref + "." + e.cfg.Domain + "/auth/v1/verify?" + q.Encode()
	err := e.client.SendAuthMessage(ctx, edgeapi.AuthMessage{Ref: p.cfg.Ref, Channel: edgeapi.ChannelEmail, Kind: kind, To: to, Code: code.Code, Link: link})
	var se *edgeapi.StatusError
	if errors.As(err, &se) && se.Status == http.StatusTooManyRequests {
		return errEmailRate
	}
	return err
}

// tooSoon refuses a code for target if one went out within emailEvery.
func tooSoon(ctx context.Context, tx pgx.Tx, target string, kinds []string) (*apiErr, error) {
	last, err := projauth.LastCodeAt(ctx, tx, target, kinds)
	if err != nil || last == nil {
		return nil, err
	}
	if wait := emailEvery - time.Since(*last); wait > 0 {
		a := refuse(http.StatusTooManyRequests, "over_email_send_rate_limit",
			fmt.Sprintf("a code was just sent to this address; wait %d seconds", int(wait.Seconds())+1))
		a.retryAfter = int(wait.Seconds()) + 1
		return a, nil
	}
	return nil, nil
}

// ---- Sign-up and sign-in --------------------------------------------------------

func (e *Edge) signup(c *call) {
	var in struct {
		Email        string          `json:"email"`
		Phone        string          `json:"phone"`
		Channel      string          `json:"channel"`
		Password     string          `json:"password"`
		Data         json.RawMessage `json:"data"`
		RedirectTo   string          `json:"redirect_to"`
		Security     security        `json:"gotrue_meta_security"`
		CaptchaToken string          `json:"captcha_token"`
	}
	if !authBody(c, &in) {
		return
	}
	a := c.p.cfg.Auth
	email := projauth.NormalizeEmail(in.Email)
	switch {
	case !a.SignupEnabled:
		c.fail(http.StatusForbidden, "signup_disabled", "sign-ups are turned off for this project")
		return
	case !jsonObject(in.Data):
		c.fail(http.StatusBadRequest, "invalid_body", "data must be a JSON object")
		return
	case !e.captcha(c, in.Security, in.CaptchaToken):
		return
	case in.Email == "" && in.Phone == "" && in.Password == "":
		e.signinAnonymous(c, in.Data)
		return
	case in.Phone != "" && in.Email == "":
		e.signupPhone(c, in.Phone, in.Channel, in.Password, in.Data)
		return
	case !validEmail(email):
		c.fail(http.StatusBadRequest, "invalid_email", "a valid email address is required")
		return
	}
	if msg := passwordProblem(a, in.Password); msg != "" {
		c.fail(http.StatusUnprocessableEntity, "weak_password", msg)
		return
	}
	redirect, ok := redirectFor(a, in.RedirectTo)
	if !ok {
		c.fail(http.StatusBadRequest, "redirect_not_allowed", "redirect_to isn't one of the project's redirect URLs")
		return
	}
	if !jsonObject(in.Data) {
		c.fail(http.StatusBadRequest, "invalid_body", "data must be a JSON object")
		return
	}
	var hash string
	var herr error
	e.hash(func() { hash, herr = projauth.HashPassword(in.Password) })
	if herr != nil {
		e.dbError(c, herr)
		return
	}
	var out *tokenResponse
	ctx := c.r.Context()
	err := e.authTx(c, func(tx pgx.Tx) error {
		existing, err := projauth.UserByEmail(ctx, tx, email, true)
		if err != nil && !errors.Is(err, projauth.ErrNotFound) {
			return err
		}
		if existing != nil {
			if !a.EmailConfirm || existing.EmailConfirmedAt != nil {
				if !a.EmailConfirm {
					return projauth.ErrExists
				}
				return nil // confirmed already: answer as for a new address
			}
			// Unconfirmed: whoever signed up before may not be the address's
			// owner, so this request's password replaces theirs and their
			// sessions end. The confirmation goes again (within the rate);
			// a code sent before keeps no password now.
			if existing.PhoneConfirmedAt == nil {
				if _, err := projauth.UpdateUser(ctx, tx, existing.ID, projauth.Update{PasswordHash: &hash}); err != nil {
					return err
				}
				if _, err := projauth.EndSessions(ctx, tx, existing.ID, nil); err != nil {
					return err
				}
			}
			if soon, err := tooSoon(ctx, tx, email, []string{projauth.CodeSignup}); err != nil || soon != nil {
				return err
			}
			var code projauth.Code
			if existing.PhoneConfirmedAt == nil {
				code, err = projauth.NewPasswordCode(ctx, tx, existing.ID, projauth.CodeSignup, email, projauth.CodeTTL, hash)
			} else {
				code, err = projauth.NewCode(ctx, tx, existing.ID, projauth.CodeSignup, email, projauth.CodeTTL)
			}
			if err != nil {
				return err
			}
			return e.sendEmail(ctx, c.p, edgeapi.EmailConfirmation, "signup", email, code, redirect)
		}
		u, err := e.createUser(ctx, c, tx, projauth.NewUser{Email: email, PasswordHash: hash, EmailConfirmed: !a.EmailConfirm,
			UserMetadata: in.Data}, "password")
		if err != nil {
			return err
		}
		if err := projauth.EnsureEmailIdentity(ctx, tx, u.ID, email); err != nil {
			return err
		}
		if a.EmailConfirm {
			code, err := projauth.NewPasswordCode(ctx, tx, u.ID, projauth.CodeSignup, email, projauth.CodeTTL, hash)
			if err != nil {
				return err
			}
			return e.sendEmail(ctx, c.p, edgeapi.EmailConfirmation, "signup", email, code, redirect)
		}
		t, err := e.startSession(ctx, c, tx, u, "password")
		out = &t
		return err
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if out != nil {
		c.json(http.StatusOK, out)
		return
	}
	// Confirmation required: the same answer whether or not the address
	// was new, so sign-up can't be used to find users.
	c.json(http.StatusOK, map[string]any{"confirmation_sent": true})
}

func jsonObject(b json.RawMessage) bool {
	s := strings.TrimSpace(string(b))
	return s == "" || s == "null" || strings.HasPrefix(s, "{")
}

func (e *Edge) signinPassword(c *call) {
	var in struct {
		Email        string   `json:"email"`
		Phone        string   `json:"phone"`
		Password     string   `json:"password"`
		Security     security `json:"gotrue_meta_security"`
		CaptchaToken string   `json:"captcha_token"`
	}
	if !authBody(c, &in) {
		return
	}
	email := projauth.NormalizeEmail(in.Email)
	var phone string
	if email == "" && in.Phone != "" {
		var err error
		if phone, err = messaging.Normalize(in.Phone); err != nil {
			c.fail(http.StatusBadRequest, "invalid_credentials", "invalid phone or password")
			return
		}
	}
	if (email == "" && phone == "") || in.Password == "" || len(in.Password) > maxPassLen {
		c.fail(http.StatusBadRequest, "invalid_credentials", "email (or phone) and password are required")
		return
	}
	if !e.captcha(c, in.Security, in.CaptchaToken) {
		return
	}
	var out tokenResponse
	var fail *apiErr
	ctx := c.r.Context()
	err := e.authTx(c, func(tx pgx.Tx) error {
		var u *projauth.User
		var err error
		if phone != "" {
			u, err = projauth.UserByPhone(ctx, tx, phone, true)
		} else {
			u, err = projauth.UserByEmail(ctx, tx, email, true)
		}
		if err != nil && !errors.Is(err, projauth.ErrNotFound) {
			return err
		}
		now := time.Now()
		if u != nil && u.LockedUntil != nil && u.LockedUntil.After(now) {
			fail = refuse(http.StatusTooManyRequests, "user_locked", "too many failed sign-ins; try again later")
			fail.retryAfter = int(u.LockedUntil.Sub(now).Seconds()) + 1
			return nil
		}
		var ok bool
		e.hash(func() { ok = projauth.CheckPassword(u, in.Password) })
		if !ok {
			fail = refuse(http.StatusBadRequest, "invalid_credentials", "invalid login credentials")
			if u == nil {
				return nil
			}
			locked, err := projauth.RecordFailedSignIn(ctx, tx, u)
			if err != nil {
				return err
			}
			if err := projauth.Audit(ctx, tx, &u.ID, projauth.ActSignInFailed, c.ip, nil); err != nil {
				return err
			}
			if locked != nil {
				return projauth.Audit(ctx, tx, &u.ID, projauth.ActLocked, c.ip, map[string]any{"until": locked})
			}
			return nil
		}
		if u.Banned(now) {
			fail = refuse(http.StatusForbidden, "user_banned", "this user is banned")
			return nil
		}
		if phone == "" && c.p.cfg.Auth.EmailConfirm && u.EmailConfirmedAt == nil {
			fail = refuse(http.StatusForbidden, "email_not_confirmed", "confirm the email address first")
			return nil
		}
		if phone != "" && c.p.cfg.Auth.PhoneConfirm && u.PhoneConfirmedAt == nil {
			fail = refuse(http.StatusForbidden, "phone_not_confirmed", "confirm the phone number first")
			return nil
		}
		// A bcrypt hash brought from Supabase becomes argon2id now that the
		// password is known (V4 §9).
		if projauth.NeedsRehash(u) {
			var rerr error
			e.hash(func() { rerr = projauth.Rehash(ctx, tx, u, in.Password) })
			if rerr != nil {
				return rerr
			}
			if err := projauth.Audit(ctx, tx, &u.ID, projauth.ActPasswordRehashed, c.ip, map[string]any{"from": "bcrypt"}); err != nil {
				return err
			}
		}
		out, err = e.startSession(ctx, c, tx, u, "password")
		return err
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, out)
}

// signinOTP sends a magic link with a 6-digit code (V4 §4.1). For an
// unknown address it creates the user when allowed; either way the answer
// is the same.
func (e *Edge) signinOTP(c *call) {
	var in struct {
		Email        string          `json:"email"`
		Phone        string          `json:"phone"`
		Channel      string          `json:"channel"`
		CreateUser   *bool           `json:"create_user"`
		Data         json.RawMessage `json:"data"`
		RedirectTo   string          `json:"redirect_to"`
		Security     security        `json:"gotrue_meta_security"`
		CaptchaToken string          `json:"captcha_token"`
	}
	if !authBody(c, &in) {
		return
	}
	a := c.p.cfg.Auth
	email := projauth.NormalizeEmail(in.Email)
	if !jsonObject(in.Data) {
		c.fail(http.StatusBadRequest, "invalid_body", "data must be a JSON object")
		return
	}
	if !e.captcha(c, in.Security, in.CaptchaToken) {
		return
	}
	if in.Phone != "" && in.Email == "" {
		e.signinPhoneOTP(c, in.Phone, in.Channel, in.CreateUser == nil || *in.CreateUser, in.Data)
		return
	}
	switch {
	case !a.MagicLinkEnabled:
		c.fail(http.StatusForbidden, "otp_disabled", "magic links and email codes are turned off for this project")
		return
	case !validEmail(email):
		c.fail(http.StatusBadRequest, "invalid_email", "a valid email address is required")
		return
	case !jsonObject(in.Data):
		c.fail(http.StatusBadRequest, "invalid_body", "data must be a JSON object")
		return
	}
	redirect, ok := redirectFor(a, in.RedirectTo)
	if !ok {
		c.fail(http.StatusBadRequest, "redirect_not_allowed", "redirect_to isn't one of the project's redirect URLs")
		return
	}
	create := in.CreateUser == nil || *in.CreateUser
	ctx := c.r.Context()
	var fail *apiErr
	err := e.authTx(c, func(tx pgx.Tx) error {
		if soon, err := tooSoon(ctx, tx, email, []string{projauth.CodeMagicLink}); err != nil || soon != nil {
			fail = soon
			return err
		}
		u, err := projauth.UserByEmail(ctx, tx, email, true)
		if errors.Is(err, projauth.ErrNotFound) {
			if !create || !a.SignupEnabled {
				return nil
			}
			if u, err = e.createUser(ctx, c, tx, projauth.NewUser{Email: email, UserMetadata: in.Data}, "magiclink"); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if u.Banned(time.Now()) {
			return nil
		}
		code, err := projauth.NewCode(ctx, tx, u.ID, projauth.CodeMagicLink, email, projauth.CodeTTL)
		if err != nil {
			return err
		}
		return e.sendEmail(ctx, c.p, edgeapi.EmailMagicLink, "magiclink", email, code, redirect)
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, map[string]any{"sent": true})
}

// verifyKinds are the code kinds a verify type accepts.
func verifyKinds(typ string) []string {
	switch typ {
	case "signup":
		return []string{projauth.CodeSignup}
	case "magiclink":
		return []string{projauth.CodeMagicLink}
	case "email":
		return []string{projauth.CodeMagicLink, projauth.CodeSignup}
	case "recovery":
		return []string{projauth.CodeRecovery}
	case "invite":
		return []string{projauth.CodeInvite}
	case "email_change":
		return []string{projauth.CodeEmailChange}
	case "sms", "whatsapp", "phone":
		return []string{projauth.CodePhone, projauth.CodePhoneSignup}
	case "phone_change":
		return []string{projauth.CodePhoneChange}
	}
	return nil
}

// useVerified signs in the user a code or link was for, applying what it
// proved (the address, a new address).
func (e *Edge) useVerified(ctx context.Context, c *call, tx pgx.Tx, v projauth.Verified) (tokenResponse, *apiErr, error) {
	u, err := projauth.GetUser(ctx, tx, v.UserID, true)
	if errors.Is(err, projauth.ErrNotFound) {
		return tokenResponse{}, refuse(http.StatusForbidden, "otp_expired", "the code is invalid or has expired"), nil
	}
	if err != nil {
		return tokenResponse{}, nil, err
	}
	if u.Banned(time.Now()) {
		return tokenResponse{}, refuse(http.StatusForbidden, "user_banned", "this user is banned"), nil
	}
	if v.Kind == projauth.CodePhone || v.Kind == projauth.CodePhoneSignup || v.Kind == projauth.CodePhoneChange {
		return e.usePhoneVerified(ctx, c, tx, u, v)
	}
	up := projauth.Update{ConfirmEmail: true}
	action := projauth.ActConfirmed
	// A magic link or reset proves the address for the first time: a
	// password (and sessions) set before by whoever signed up with it
	// unconfirmed must not come with it (pre-registration takeover). A
	// sign-up confirmation or an invitation keeps only the password the
	// request that sent it set: not a squatter's, kept through the owner's
	// own sign-up or an invitation.
	drop := v.Kind == projauth.CodeMagicLink || v.Kind == projauth.CodeRecovery
	if v.Kind == projauth.CodeSignup || v.Kind == projauth.CodeInvite {
		drop = u.PhoneConfirmedAt == nil && u.HasPassword() && !v.KeepsPassword(u)
	}
	if u.EmailConfirmedAt == nil && drop {
		if u.HasPassword() {
			none := ""
			up.PasswordHash = &none
		}
		if _, err := projauth.EndSessions(ctx, tx, u.ID, nil); err != nil {
			return tokenResponse{}, nil, err
		}
	}
	if v.Kind == projauth.CodeEmailChange {
		up.Email, action, up.NotAnonymous = &v.Target, projauth.ActEmailChanged, true
	} else if u.Email == nil || *u.Email != v.Target {
		// The address changed since the code was sent.
		return tokenResponse{}, refuse(http.StatusForbidden, "otp_expired", "the code is invalid or has expired"), nil
	}
	if u, err = projauth.UpdateUser(ctx, tx, u.ID, up); err != nil {
		if errors.Is(err, projauth.ErrExists) {
			return tokenResponse{}, refuse(http.StatusUnprocessableEntity, "email_exists", "another user has this email address now"), nil
		}
		return tokenResponse{}, nil, err
	}
	if v.Kind == projauth.CodeEmailChange || u.EmailConfirmedAt != nil {
		if err := projauth.EnsureEmailIdentity(ctx, tx, u.ID, v.Target); err != nil {
			return tokenResponse{}, nil, err
		}
	}
	if err := projauth.Audit(ctx, tx, &u.ID, action, c.ip, map[string]any{"via": v.Kind}); err != nil {
		return tokenResponse{}, nil, err
	}
	method := map[string]string{projauth.CodeSignup: "email/signup", projauth.CodeMagicLink: "otp", projauth.CodeRecovery: "recovery",
		projauth.CodeInvite: "invite", projauth.CodeEmailChange: "email_change"}[v.Kind]
	t, err := e.startSession(ctx, c, tx, u, method)
	return t, nil, err
}

func (e *Edge) verify(c *call) {
	var in struct {
		Type      string `json:"type"`
		Email     string `json:"email"`
		Phone     string `json:"phone"`
		Token     string `json:"token"`
		TokenHash string `json:"token_hash"`
	}
	if !authBody(c, &in) {
		return
	}
	kinds := verifyKinds(in.Type)
	if kinds == nil {
		c.fail(http.StatusBadRequest, "invalid_type", "type is signup, magiclink, email, recovery, invite, email_change, sms, whatsapp or phone_change")
		return
	}
	target := in.Email
	if kinds[0] == projauth.CodePhone || kinds[0] == projauth.CodePhoneChange {
		phone, err := messaging.Normalize(in.Phone)
		if err != nil || in.Token == "" {
			c.fail(http.StatusBadRequest, "invalid_body", "send phone and token (the 6-digit code)")
			return
		}
		target, in.TokenHash = phone, ""
	} else if in.TokenHash == "" && (in.Email == "" || in.Token == "") {
		c.fail(http.StatusBadRequest, "invalid_body", "send email and token (the 6-digit code), or token_hash (the link's token)")
		return
	}
	ctx := c.r.Context()
	var out tokenResponse
	var fail *apiErr
	err := e.authTx(c, func(tx pgx.Tx) error {
		var v projauth.Verified
		var err error
		if in.TokenHash != "" {
			v, err = projauth.VerifyToken(ctx, tx, in.TokenHash, kinds)
		} else {
			v, err = projauth.VerifyCode(ctx, tx, target, kinds, in.Token)
		}
		if err != nil {
			return err
		}
		if !v.OK {
			fail = refuse(http.StatusForbidden, "otp_expired", "the code is invalid or has expired")
			return nil
		}
		out, fail, err = e.useVerified(ctx, c, tx, v)
		return err
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, out)
}

func (e *Edge) resend(c *call) {
	var in struct {
		Type       string `json:"type"`
		Email      string `json:"email"`
		Phone      string `json:"phone"`
		Channel    string `json:"channel"`
		RedirectTo string `json:"redirect_to"`
	}
	if !authBody(c, &in) {
		return
	}
	if in.Type == "sms" || in.Type == "whatsapp" {
		ch := in.Channel
		if ch == "" && in.Type == "whatsapp" {
			ch = edgeapi.ChannelWhatsApp
		}
		e.signinPhoneOTP(c, in.Phone, ch, false, nil)
		return
	}
	if in.Type != "signup" {
		c.fail(http.StatusBadRequest, "invalid_type", "type is signup or sms (a change is resent by asking for it again)")
		return
	}
	email := projauth.NormalizeEmail(in.Email)
	redirect, ok := redirectFor(c.p.cfg.Auth, in.RedirectTo)
	if !ok {
		c.fail(http.StatusBadRequest, "redirect_not_allowed", "redirect_to isn't one of the project's redirect URLs")
		return
	}
	ctx := c.r.Context()
	var fail *apiErr
	err := e.authTx(c, func(tx pgx.Tx) error {
		if soon, err := tooSoon(ctx, tx, email, []string{projauth.CodeSignup}); err != nil || soon != nil {
			fail = soon
			return err
		}
		u, err := projauth.UserByEmail(ctx, tx, email, false)
		if errors.Is(err, projauth.ErrNotFound) || (err == nil && u.EmailConfirmedAt != nil) {
			return nil
		}
		if err != nil {
			return err
		}
		code, err := projauth.NewCode(ctx, tx, u.ID, projauth.CodeSignup, email, projauth.CodeTTL)
		if err != nil {
			return err
		}
		return e.sendEmail(ctx, c.p, edgeapi.EmailConfirmation, "signup", email, code, redirect)
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, map[string]any{"sent": true})
}

func (e *Edge) recover(c *call) {
	var in struct {
		Email      string `json:"email"`
		RedirectTo string `json:"redirect_to"`
	}
	if !authBody(c, &in) {
		return
	}
	email := projauth.NormalizeEmail(in.Email)
	if !validEmail(email) {
		c.fail(http.StatusBadRequest, "invalid_email", "a valid email address is required")
		return
	}
	redirect, ok := redirectFor(c.p.cfg.Auth, in.RedirectTo)
	if !ok {
		c.fail(http.StatusBadRequest, "redirect_not_allowed", "redirect_to isn't one of the project's redirect URLs")
		return
	}
	ctx := c.r.Context()
	var fail *apiErr
	err := e.authTx(c, func(tx pgx.Tx) error {
		if soon, err := tooSoon(ctx, tx, email, []string{projauth.CodeRecovery}); err != nil || soon != nil {
			fail = soon
			return err
		}
		u, err := projauth.UserByEmail(ctx, tx, email, false)
		if errors.Is(err, projauth.ErrNotFound) {
			return nil // the same answer: recovery can't find users
		}
		if err != nil {
			return err
		}
		if u.Banned(time.Now()) {
			return nil
		}
		code, err := projauth.NewCode(ctx, tx, u.ID, projauth.CodeRecovery, email, projauth.CodeTTL)
		if err != nil {
			return err
		}
		if err := projauth.Audit(ctx, tx, &u.ID, projauth.ActRecovery, c.ip, nil); err != nil {
			return err
		}
		return e.sendEmail(ctx, c.p, edgeapi.EmailRecovery, "recovery", email, code, redirect)
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, map[string]any{"sent": true})
}

// refresh rotates a refresh token (V4 §4.4).
func (e *Edge) refresh(c *call) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !authBody(c, &in) {
		return
	}
	if in.RefreshToken == "" || len(in.RefreshToken) > 200 {
		c.fail(http.StatusBadRequest, "invalid_refresh_token", "refresh_token is required")
		return
	}
	ctx := c.r.Context()
	var out tokenResponse
	var fail *apiErr
	err := e.authTx(c, func(tx pgx.Tx) error {
		r, err := projauth.Refresh(ctx, tx, in.RefreshToken,
			projauth.SessionLimits{Inactivity: time.Duration(c.p.cfg.Auth.SessionInactivitySeconds) * time.Second})
		if errors.Is(err, projauth.ErrNotFound) {
			fail = refuse(http.StatusBadRequest, "invalid_refresh_token", "the refresh token is invalid or its session has ended")
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case r.Reused:
			fail = refuse(http.StatusBadRequest, "refresh_token_reused",
				"this refresh token was already used, so its session was signed out (it may have been stolen); sign in again")
			return projauth.Audit(ctx, tx, &r.Session.UserID, projauth.ActTokenReused, c.ip, map[string]any{"session_id": r.Session.ID})
		case r.Expired:
			fail = refuse(http.StatusUnauthorized, "session_expired", "the session has ended; sign in again")
			return nil
		case r.Banned:
			fail = refuse(http.StatusForbidden, "user_banned", "this user is banned")
			return nil
		}
		if err := e.mauAllowed(c, r.User.ID); err != nil {
			return err
		}
		e.meter.activeUser(c.p.cfg.ProjectID, r.User.ID)
		c.userID = &r.User.ID
		out, err = e.issue(ctx, c.p, r.User, r.Session, r.Token)
		return err
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, out)
}

// sessionOf is the signed-in user's id and session from their token.
func sessionOf(c *call, req Request) (uuid.UUID, uuid.UUID, bool) {
	if req.Role != "user" || c.userID == nil {
		c.fail(http.StatusUnauthorized, "not_signed_in", "send the user's access token (Authorization: Bearer …)")
		return uuid.Nil, uuid.Nil, false
	}
	sid, _ := req.Claims["session_id"].(string)
	s, err := uuid.Parse(sid)
	if err != nil {
		c.fail(http.StatusUnauthorized, "invalid_token", "the token has no session")
		return uuid.Nil, uuid.Nil, false
	}
	return *c.userID, s, true
}

func (e *Edge) signout(c *call, req Request) {
	uid, sid, ok := sessionOf(c, req)
	if !ok {
		return
	}
	scope := c.r.URL.Query().Get("scope")
	if scope == "" {
		scope = "local"
	}
	if scope != "local" && scope != "global" && scope != "others" {
		c.fail(http.StatusBadRequest, "invalid_scope", "scope is local, global or others")
		return
	}
	ctx := c.r.Context()
	err := e.authTx(c, func(tx pgx.Tx) error {
		var err error
		switch scope {
		case "local":
			err = projauth.EndSession(ctx, tx, sid)
		case "global":
			_, err = projauth.EndSessions(ctx, tx, uid, nil)
		default:
			_, err = projauth.EndSessions(ctx, tx, uid, &sid)
		}
		if err != nil {
			return err
		}
		return projauth.Audit(ctx, tx, &uid, projauth.ActSignOut, c.ip, map[string]any{"scope": scope})
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	c.w.WriteHeader(http.StatusNoContent)
}

// liveUser is the token's user while its session lasts.
func (e *Edge) liveUser(ctx context.Context, tx pgx.Tx, uid, sid uuid.UUID, lock bool) (*projauth.User, *apiErr, error) {
	if _, err := projauth.GetSession(ctx, tx, sid); errors.Is(err, projauth.ErrNotFound) {
		return nil, refuse(http.StatusUnauthorized, "session_not_found", "the session has ended; sign in again"), nil
	} else if err != nil {
		return nil, nil, err
	}
	u, err := projauth.GetUser(ctx, tx, uid, lock)
	if errors.Is(err, projauth.ErrNotFound) {
		return nil, refuse(http.StatusUnauthorized, "user_not_found", "the user no longer exists"), nil
	}
	return u, nil, err
}

func (e *Edge) getUser(c *call, req Request) {
	uid, sid, ok := sessionOf(c, req)
	if !ok {
		return
	}
	ctx := c.r.Context()
	var out userOut
	var fail *apiErr
	err := e.authTx(c, func(tx pgx.Tx) error {
		u, f, err := e.liveUser(ctx, tx, uid, sid, false)
		if err != nil || f != nil {
			fail = f
			return err
		}
		out, err = e.userWithIdentities(c, tx, u)
		return err
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, out)
}

// mergeObject overlays b's top-level keys on a (both JSON objects).
func mergeObject(a, b json.RawMessage) (json.RawMessage, error) {
	m := map[string]any{}
	if len(a) > 0 {
		if err := json.Unmarshal(a, &m); err != nil {
			return nil, err
		}
	}
	var add map[string]any
	if err := json.Unmarshal(b, &add); err != nil {
		return nil, err
	}
	for k, v := range add {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

func (e *Edge) updateUser(c *call, req Request) {
	uid, sid, ok := sessionOf(c, req)
	if !ok {
		return
	}
	var in struct {
		Email           *string         `json:"email"`
		Phone           *string         `json:"phone"`
		Channel         string          `json:"channel"`
		Password        *string         `json:"password"`
		Data            json.RawMessage `json:"data"`
		EmailRedirectTo string          `json:"email_redirect_to"`
	}
	if !authBody(c, &in) {
		return
	}
	a := c.p.cfg.Auth
	var hash *string
	if in.Password != nil {
		if msg := passwordProblem(a, *in.Password); msg != "" {
			c.fail(http.StatusUnprocessableEntity, "weak_password", msg)
			return
		}
		var h string
		var herr error
		e.hash(func() { h, herr = projauth.HashPassword(*in.Password) })
		if herr != nil {
			e.dbError(c, herr)
			return
		}
		hash = &h
	}
	if in.Data != nil && !strings.HasPrefix(strings.TrimSpace(string(in.Data)), "{") {
		c.fail(http.StatusBadRequest, "invalid_body", "data must be a JSON object")
		return
	}
	var newEmail string
	if in.Email != nil {
		newEmail = projauth.NormalizeEmail(*in.Email)
		if !validEmail(newEmail) {
			c.fail(http.StatusBadRequest, "invalid_email", "a valid email address is required")
			return
		}
	}
	var newPhone, channel string
	if in.Phone != nil {
		if !phoneOn(c) {
			return
		}
		if newPhone, ok = phoneIn(c, *in.Phone); !ok {
			return
		}
		if channel, ok = channelIn(c, in.Channel); !ok {
			return
		}
	}
	redirect, ok := redirectFor(a, in.EmailRedirectTo)
	if !ok {
		c.fail(http.StatusBadRequest, "redirect_not_allowed", "email_redirect_to isn't one of the project's redirect URLs")
		return
	}
	ctx := c.r.Context()
	var u *projauth.User
	var fail *apiErr
	err := e.authTx(c, func(tx pgx.Tx) error {
		var err error
		if u, fail, err = e.liveUser(ctx, tx, uid, sid, true); err != nil || fail != nil {
			return err
		}
		up := projauth.Update{PasswordHash: hash}
		if in.Data != nil {
			if up.UserMetadata, err = mergeObject(u.UserMetadata, in.Data); err != nil {
				return err
			}
		}
		if hash != nil || in.Data != nil {
			if u, err = projauth.UpdateUser(ctx, tx, uid, up); err != nil {
				return err
			}
			action := projauth.ActUpdated
			if hash != nil {
				action = projauth.ActPasswordChange
				// A new password signs out every other session (ASVS 3.3.3):
				// whoever knew the old one is out.
				if _, err := projauth.EndSessions(ctx, tx, uid, &sid); err != nil {
					return err
				}
			}
			if err := projauth.Audit(ctx, tx, &uid, action, c.ip, nil); err != nil {
				return err
			}
		}
		if newEmail != "" && (u.Email == nil || *u.Email != newEmail) {
			if other, err := projauth.UserByEmail(ctx, tx, newEmail, false); err == nil && other.ID != uid {
				fail = refuse(http.StatusUnprocessableEntity, "email_exists", "another user has this email address")
				return nil
			} else if err != nil && !errors.Is(err, projauth.ErrNotFound) {
				return err
			}
			if soon, err := tooSoon(ctx, tx, newEmail, []string{projauth.CodeEmailChange}); err != nil || soon != nil {
				fail = soon
				return err
			}
			code, err := projauth.NewCode(ctx, tx, uid, projauth.CodeEmailChange, newEmail, projauth.CodeTTL)
			if err != nil {
				return err
			}
			if err := e.sendEmail(ctx, c.p, edgeapi.EmailChange, "email_change", newEmail, code, redirect); err != nil {
				return err
			}
		}
		if newPhone != "" && (u.Phone == nil || *u.Phone != newPhone) {
			if other, err := projauth.UserByPhone(ctx, tx, newPhone, false); err == nil && other.ID != uid {
				fail = refuse(http.StatusUnprocessableEntity, "phone_exists", "another user has this phone number")
				return nil
			} else if err != nil && !errors.Is(err, projauth.ErrNotFound) {
				return err
			}
			if soon, err := phoneTooSoon(ctx, tx, newPhone, []string{projauth.CodePhoneChange}); err != nil || soon != nil {
				fail = soon
				return err
			}
			return e.newPhoneCode(ctx, c, tx, uid, projauth.CodePhoneChange, channel, newPhone, "")
		}
		return nil
	})
	if err != nil {
		e.authDBError(c, err)
		return
	}
	if fail != nil {
		fail.send(c)
		return
	}
	c.json(http.StatusOK, u)
}
