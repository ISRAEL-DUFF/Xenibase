package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/mail"
	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) step()          { c.mu.Lock(); c.t = c.t.Add(30 * time.Second); c.mu.Unlock() }

// browser is an HTTP client with a cookie jar that sends the CSRF token
// like the web UI does.
type browser struct {
	t    *testing.T
	base string
	c    *http.Client
	csrf string
}

func (b *browser) do(method, path string, body any, out any, hdr ...string) (int, string) {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rd = bytes.NewReader(buf)
	}
	req, _ := http.NewRequest(method, b.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if b.csrf != "" {
		req.Header.Set("X-CSRF-Token", b.csrf)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	return res.StatusCode, string(raw)
}

func (b *browser) session() gen.SessionState {
	var st gen.SessionState
	if code, body := b.do("GET", "/api/v1/session", nil, &st); code != 200 {
		b.t.Fatalf("session: %d %s", code, body)
	}
	b.csrf = st.CsrfToken
	return st
}

func newAuthServer(t *testing.T) (*httptest.Server, *fakeClock, DB) {
	t.Helper()
	pool := storetest.New(t)
	key, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(key)
	clk := &fakeClock{t: time.Now()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.NewService(pool, kr, auth.Config{Now: clk.Now, ReauthWindow: time.Minute}, "let-me-in", log)
	if err := svc.EnsureTerms(context.Background()); err != nil {
		t.Fatal(err)
	}
	orgSvc := orgs.New(pool, svc, nil, nil, "", log)
	svc.SetHooks(orgSvc.Hooks())
	ts := httptest.NewServer(NewHandler(Options{
		Logger: log, DB: pool, Auth: svc, DevEndpoints: true, Orgs: orgSvc, Mail: mail.New(pool, kr),
		UI: fstest.MapFS{"index.html": {Data: []byte("app")}}, UIIndex: "index.html",
	}))
	t.Cleanup(ts.Close)
	return ts, clk, pool
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, c: &http.Client{Jar: jar}}
}

const (
	ownerEmail    = "owner@example.com"
	ownerPassword = "correct horse battery staple"
)

func (b *browser) setup(clk *fakeClock) string {
	b.t.Helper()
	b.session()
	var enr gen.SetupEnrollment
	if code, body := b.do("POST", "/api/v1/setup/begin", map[string]string{
		"setup_code": "let-me-in", "email": ownerEmail, "password": ownerPassword,
	}, &enr); code != 200 {
		b.t.Fatalf("setup begin: %d %s", code, body)
	}
	totp, _ := auth.TOTPCode(enr.TotpSecret, clk.Now())
	var st gen.SessionState
	if code, body := b.do("POST", "/api/v1/setup/complete", map[string]string{
		"enrollment_token": enr.EnrollmentToken, "code": totp,
	}, &st); code != 200 || !st.Authenticated {
		b.t.Fatalf("setup complete: %d %s", code, body)
	}
	clk.step()
	return enr.TotpSecret
}

func TestAuthFlowOverHTTP(t *testing.T) {
	ts, clk, db := newAuthServer(t)
	b := newBrowser(t, ts.URL)

	st := b.session()
	if !st.SetupRequired || st.Authenticated || len(st.CsrfToken) < 32 {
		t.Fatalf("fresh session state: %+v", st)
	}

	// Protected routes need a session.
	if code, _ := b.do("GET", "/api/v1/operations", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("operations without session: %d", code)
	}
	// Public routes stay public; the SPA is never gated.
	if code, _ := b.do("GET", "/api/v1/version", nil, nil); code != 200 {
		t.Fatalf("version: %d", code)
	}
	if code, _ := b.do("GET", "/projects/xyz", nil, nil); code != 200 {
		t.Fatalf("spa: %d", code)
	}

	// Mutations need the CSRF token.
	saved := b.csrf
	b.csrf = ""
	if code, body := b.do("POST", "/api/v1/setup/begin", map[string]string{}, nil); code != http.StatusForbidden || !strings.Contains(body, "csrf") {
		t.Fatalf("no csrf: %d %s", code, body)
	}
	b.csrf = "wrong-" + saved
	if code, _ := b.do("POST", "/api/v1/setup/begin", map[string]string{}, nil); code != http.StatusForbidden {
		t.Fatalf("bad csrf: %d", code)
	}
	b.csrf = saved
	if code, _ := b.do("POST", "/api/v1/setup/begin", map[string]string{"setup_code": "x"}, nil, "Origin", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", code)
	}
	if code, body := b.do("POST", "/api/v1/setup/begin", map[string]string{
		"setup_code": "wrong", "email": ownerEmail, "password": ownerPassword,
	}, nil); code != http.StatusForbidden || !strings.Contains(body, "bad_setup_code") {
		t.Fatalf("wrong setup code: %d %s", code, body)
	}

	secret := b.setup(clk)
	if st := b.session(); !st.Authenticated || st.SetupRequired || st.User == nil || string(st.User.Email) != ownerEmail || st.TermsRequired != nil {
		t.Fatalf("after setup: %+v", st)
	}
	var me gen.User
	if code, _ := b.do("GET", "/api/v1/me", nil, &me); code != 200 || me.PlatformRole != gen.UserPlatformRolePlatformAdmin {
		t.Fatalf("me: %d %+v", code, me)
	}

	// Setup cannot run twice.
	other := newBrowser(t, ts.URL)
	other.session()
	if code, _ := other.do("POST", "/api/v1/setup/begin", map[string]string{
		"setup_code": "let-me-in", "email": "x@example.com", "password": ownerPassword,
	}, nil); code != http.StatusConflict {
		t.Fatalf("second setup: %d", code)
	}

	// Log out, then back in with password + TOTP.
	if code, _ := b.do("POST", "/api/v1/auth/logout", nil, nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if code, _ := b.do("GET", "/api/v1/me", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("me after logout: %d", code)
	}
	var ch gen.LoginChallenge
	if code, body := b.do("POST", "/api/v1/auth/login", map[string]string{"email": ownerEmail, "password": ownerPassword}, &ch); code != 200 {
		t.Fatalf("login: %d %s", code, body)
	}
	if code, _ := b.do("GET", "/api/v1/me", nil, nil); code != http.StatusUnauthorized {
		t.Fatal("password alone must not sign in")
	}
	totp, _ := auth.TOTPCode(secret, clk.Now())
	if code, body := b.do("POST", "/api/v1/auth/totp", map[string]string{"challenge_id": *ch.ChallengeId, "code": totp}, nil); code != 200 {
		t.Fatalf("totp: %d %s", code, body)
	}
	clk.step()

	// Destructive actions need a recent re-auth.
	for range 3 {
		clk.step() // past the 1-minute window
	}
	path := "/api/v1/projects/00000000-0000-0000-0000-000000000001?confirm=x"
	if code, body := b.do("DELETE", path, nil, nil); code != http.StatusForbidden || !strings.Contains(body, "reauth_required") {
		t.Fatalf("delete without reauth: %d %s", code, body)
	}
	totp, _ = auth.TOTPCode(secret, clk.Now())
	if code, body := b.do("POST", "/api/v1/auth/reauth", map[string]string{"password": ownerPassword, "code": totp}, nil); code != http.StatusNoContent {
		t.Fatalf("reauth: %d %s", code, body)
	}
	// Past re-authentication now; no such project, so 404.
	if code, _ := b.do("DELETE", path, nil, nil); code != http.StatusNotFound {
		t.Fatalf("delete after reauth: %d", code)
	}

	// Session cookie flags.
	res, _ := http.Get(ts.URL + "/api/v1/session")
	_ = res.Body.Close()

	// Everything mutating was audited, including the refusals.
	ctx := context.Background()
	rows, err := store.New(db).ListAudit(ctx, store.ListAuditParams{Platform: true, MaxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, r := range rows {
		seen[r.Action+":"+r.Outcome]++
	}
	for _, want := range []string{"setup.complete:success", "auth.login:success", "auth.totp:success",
		"auth.reauth:success", "auth.logout:success", "project.delete:denied", "project.delete:failure", "setup.begin:denied"} {
		if seen[want] == 0 {
			t.Errorf("no audit row %s (have %v)", want, seen)
		}
	}
	var list gen.AuditList
	if code, _ := b.do("GET", "/api/v1/admin/audit?action=auth.&limit=50", nil, &list); code != 200 || len(list.Items) == 0 {
		t.Fatalf("audit api: %d %d", code, len(list.Items))
	}
	for _, e := range list.Items {
		if !strings.HasPrefix(e.Action, "auth.") {
			t.Errorf("filter leaked %s", e.Action)
		}
		if strings.Contains(strings.ToLower(jsonString(e.Detail)), "password") {
			t.Errorf("audit detail mentions a password: %v", e.Detail)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	ts, clk, _ := newAuthServer(t)
	b := newBrowser(t, ts.URL)
	b.setup(clk)
	codes := map[int]int{}
	for range 12 {
		code, _ := b.do("POST", "/api/v1/auth/login", map[string]string{"email": "x@example.com", "password": "nope nope nope"}, nil)
		codes[code]++
	}
	if codes[http.StatusTooManyRequests] == 0 {
		t.Fatalf("no rate limiting: %v", codes)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	pool := storetest.New(t)
	key, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(key)
	clk := &fakeClock{t: time.Now()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.NewService(pool, kr, auth.Config{Now: clk.Now}, "c", log)
	h := NewHandler(Options{Logger: log, DB: pool, Auth: svc, Security: SecurityOptions{SecureCookies: true},
		UI: fstest.MapFS{"index.html": {Data: []byte("app")}}, UIIndex: "index.html"})

	req := httptest.NewRequest("GET", "/api/v1/session", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var csrf *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "__Host-xenibase_csrf" {
			csrf = c
		}
	}
	if csrf == nil || !csrf.Secure || csrf.HttpOnly || csrf.SameSite != http.SameSiteStrictMode || csrf.Path != "/" {
		t.Fatalf("csrf cookie: %+v", csrf)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing security headers")
	}

	// Complete setup to inspect the session cookie.
	var enr gen.SetupEnrollment
	post := func(path string, body any, out any) *httptest.ResponseRecorder {
		buf, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(buf))
		r.AddCookie(csrf)
		r.Header.Set("X-CSRF-Token", csrf.Value)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		_ = json.Unmarshal(w.Body.Bytes(), out)
		return w
	}
	post("/api/v1/setup/begin", map[string]string{"setup_code": "c", "email": ownerEmail, "password": ownerPassword}, &enr)
	code, _ := auth.TOTPCode(enr.TotpSecret, clk.Now())
	w := post("/api/v1/setup/complete", map[string]string{"enrollment_token": enr.EnrollmentToken, "code": code}, &gen.SessionState{})
	var sess *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-xenibase_session" {
			sess = c
		}
	}
	if sess == nil || !sess.Secure || !sess.HttpOnly || sess.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie: %+v (status %d %s)", sess, w.Code, w.Body.String())
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
