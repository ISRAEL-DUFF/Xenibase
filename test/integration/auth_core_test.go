package integration

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/jwtes"
	"github.com/israel-duff/xenibase/internal/services"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// authSession is a token response.
type authSession struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	User         struct {
		ID               uuid.UUID  `json:"id"`
		Email            string     `json:"email"`
		EmailConfirmedAt *time.Time `json:"email_confirmed_at"`
	} `json:"user"`
}

type authResult struct {
	Code    int
	Body    string
	Header  http.Header
	Session authSession
	Error   struct {
		Code string `json:"code"`
	} `json:"error"`
}

// authAPI calls a project's /auth/v1 with a key.
type authAPI struct {
	t   *testing.T
	ed  *testenv.Edge
	ref string
	key string
}

func (a authAPI) call(method, path, body, token string) authResult {
	a.t.Helper()
	h := []string{}
	if a.key != "" {
		h = append(h, "apikey", a.key)
	}
	if token != "" {
		h = append(h, "Authorization", "Bearer "+token)
	}
	var code int
	var hdr http.Header
	var out string
	if body != "" {
		h = append(h, "Content-Type", "application/json")
		code, hdr, out = a.ed.Do(a.ref, method, path, strings.NewReader(body), h...)
	} else {
		code, hdr, out = a.ed.Do(a.ref, method, path, nil, h...)
	}
	r := authResult{Code: code, Body: out, Header: hdr}
	_ = json.Unmarshal([]byte(out), &r.Session)
	_ = json.Unmarshal([]byte(out), &r)
	return r
}

func (a authAPI) post(path, body string) authResult { return a.call("POST", path, body, "") }

// mailbox reads auth emails from the test SMTP server, newest first.
type mailbox struct {
	e    *testenv.Env
	seen map[string]int
}

var (
	codeRe = regexp.MustCompile(`code: (\d{6})`)
	linkRe = regexp.MustCompile(`https://\S+/auth/v1/verify\?\S+`)
)

// next is the next email to addr: its code and link.
func (m *mailbox) next(t *testing.T, addr string) (code, link string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := m.e.Services.SendAuthEmails(context.Background()); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, ml := range m.e.SMTP.Mail() {
			if len(ml.To) == 0 || !strings.EqualFold(ml.To[0], addr) {
				continue
			}
			n++
			if n <= m.seen[addr] {
				continue
			}
			m.seen[addr] = n
			if c := codeRe.FindStringSubmatch(ml.Data); c != nil {
				code = c[1]
			}
			return code, linkRe.FindString(ml.Data)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no email to %s", addr)
	return "", ""
}

// TestAuthCore is V4-M31's done-when (V4 §4): sign-up, sign-in, refresh,
// sign-out-everywhere and reuse detection pass, and RLS policies using
// xb_auth.uid() enforce per-user data for users who signed up through the
// API. Around it: email confirmation by code and by link, magic links,
// recovery, lockout, the admin API, bans and deletion, the JWKS endpoint,
// signing-key rotation, monthly active users, the dashboard's user API,
// the platform's email allowance and the project's own SMTP.
func TestAuthCore(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	creds := e.CreateProject("auth-app")
	pid := creds.Project.Id
	p, err := store.New(e.DB).GetProject(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	var en gen.BackendServicesEnabled
	if code := e.Do("POST", "/api/v1/projects/"+pid.String()+"/services", nil, &en); code != http.StatusAccepted {
		t.Fatalf("enable: %d", code)
	}
	if op := e.WaitOperation(en.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("enable: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	var pub, sec string
	for _, k := range en.Keys {
		if k.Key.Kind == gen.ApiKeyKindPublishable {
			pub = k.Value
		} else {
			sec = k.Value
		}
	}
	ref := *en.Services.Ref
	base := "/api/v1/projects/" + pid.String()

	// The app's settings: a site, a callback, a longer minimum password.
	var cfg gen.AuthConfig
	if code := e.Do("PATCH", base+"/auth/config", gen.AuthConfigUpdate{Settings: &gen.AuthSettings{
		SiteUrl: ptr("https://app.example.com"), RedirectUrls: &[]string{"https://app.example.com/callback"}, PasswordMinLength: ptr(10),
	}}, &cfg); code != 200 || *cfg.Settings.PasswordMinLength != 10 || !*cfg.Settings.EmailConfirm {
		t.Fatalf("auth config: %d %+v", code, cfg.Settings)
	}

	// The app's table: rows belong to users, and go with them.
	app := e.MustConnect(creds.Connection.PooledUrl)
	defer app.Close(ctx)
	userRole := store.UserRole(p.DbName)
	for _, stmt := range []string{
		`CREATE TABLE todos (id bigserial PRIMARY KEY, owner_id uuid NOT NULL DEFAULT xb_auth.uid() REFERENCES xb_auth.users (id) ON DELETE CASCADE,
		   title text NOT NULL)`,
		`ALTER TABLE todos ENABLE ROW LEVEL SECURITY`,
		fmt.Sprintf(`CREATE POLICY own ON todos FOR ALL TO %q USING (owner_id = xb_auth.uid()) WITH CHECK (owner_id = xb_auth.uid())`, userRole),
		`CREATE VIEW my_profile WITH (security_invoker = true) AS SELECT id, email FROM xb_auth.user_profiles`,
	} {
		if _, err := app.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// The owner can't read the auth tables themselves.
	if _, err := app.Exec(ctx, `SELECT encrypted_password FROM xb_auth.users`); err == nil {
		t.Fatal("the owner read xb_auth.users")
	}

	ed := e.StartEdge()
	api := authAPI{t: t, ed: ed, ref: ref, key: pub}
	admin := authAPI{t: t, ed: ed, ref: ref, key: sec}
	box := &mailbox{e: e, seen: map[string]int{}}
	// The edge reaches the database (the poolers know its login) …
	waitFor(t, 30*time.Second, "the edge reaches the project's database", func() bool {
		return api.call("GET", "/data/v1/health", "", "").Code == 200
	})
	// … and has the new settings once the password rule applies.
	waitFor(t, 30*time.Second, "the auth settings reach the edge", func() bool {
		r := api.call("GET", "/auth/v1/settings", "", "")
		return strings.Contains(r.Body, `"password_min_length":10`)
	})

	// ---- Sign-up with email confirmation, by code ------------------------------
	if r := api.post("/auth/v1/signup", `{"email":"Alice@Example.com","password":"short"}`); r.Code != 422 || r.Error.Code != "weak_password" {
		t.Fatalf("a short password: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signup", `{"email":"alice@example.com","password":"alice-password-1","data":{"name":"Alice"}}`); r.Code != 200 ||
		!strings.Contains(r.Body, `"confirmation_sent":true`) {
		t.Fatalf("signup: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/password", `{"email":"alice@example.com","password":"alice-password-1"}`); r.Code != 403 ||
		r.Error.Code != "email_not_confirmed" {
		t.Fatalf("sign-in before confirming: %d %s", r.Code, r.Body)
	}
	code, _ := box.next(t, "alice@example.com")
	if r := api.post("/auth/v1/verify", `{"type":"signup","email":"alice@example.com","token":"000000"}`); r.Code != 403 || r.Error.Code != "otp_expired" {
		t.Fatalf("a wrong code: %d %s", r.Code, r.Body)
	}
	r := api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"signup","email":"alice@example.com","token":%q}`, code))
	if r.Code != 200 || r.Session.AccessToken == "" || r.Session.User.EmailConfirmedAt == nil {
		t.Fatalf("confirm by code: %d %s", r.Code, r.Body)
	}
	alice := r.Session.User.ID
	if r := api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"signup","email":"alice@example.com","token":%q}`, code)); r.Code != 403 {
		t.Fatalf("a code used twice: %d %s", r.Code, r.Body)
	}

	// ---- … and by link, redirected to the app with the session ----------------
	if r := api.post("/auth/v1/signup", `{"email":"bob@example.com","password":"bob-password-12","redirect_to":"https://evil.example.net/"}`); r.Code != 400 ||
		r.Error.Code != "redirect_not_allowed" {
		t.Fatalf("a redirect not allowed: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signup", `{"email":"bob@example.com","password":"bob-password-12","redirect_to":"https://app.example.com/callback"}`); r.Code != 200 {
		t.Fatalf("signup bob: %d %s", r.Code, r.Body)
	}
	_, link := box.next(t, "bob@example.com")
	lu, err := url.Parse(link)
	if err != nil || lu.Host != ref+"."+testenv.EdgeDomain {
		t.Fatalf("link %q", link)
	}
	// The link works without a key, as from an email.
	lr := authAPI{t: t, ed: ed, ref: ref}.call("GET", lu.RequestURI(), "", "")
	loc, _ := url.Parse(lr.Header.Get("Location"))
	frag, _ := url.ParseQuery(loc.Fragment)
	if lr.Code != http.StatusSeeOther || loc.Host != "app.example.com" || loc.Path != "/callback" || frag.Get("access_token") == "" ||
		frag.Get("refresh_token") == "" || frag.Get("type") != "signup" {
		t.Fatalf("link: %d %s", lr.Code, lr.Header.Get("Location"))
	}
	bobToken := frag.Get("access_token")
	lr = authAPI{t: t, ed: ed, ref: ref}.call("GET", lu.RequestURI(), "", "")
	if loc, _ := url.Parse(lr.Header.Get("Location")); lr.Code != http.StatusSeeOther || !strings.Contains(loc.Fragment, "error_code=otp_expired") {
		t.Fatalf("a used link: %d %s", lr.Code, lr.Header.Get("Location"))
	}

	// ---- Sign-in -------------------------------------------------------------
	r = api.post("/auth/v1/signin/password", `{"email":"ALICE@example.com","password":"alice-password-1"}`)
	if r.Code != 200 || r.Session.User.ID != alice {
		t.Fatalf("sign-in: %d %s", r.Code, r.Body)
	}
	aliceSession := r.Session
	claims, err := jwtes.Verify(aliceSession.AccessToken, jwksOf(t, api), ref, time.Now())
	if err != nil || claims["sub"] != alice.String() || claims["role"] != "user" || claims["email"] != "alice@example.com" ||
		claims["session_id"] == nil || claims["user_metadata"].(map[string]any)["name"] != "Alice" {
		t.Fatalf("access token: %v %v", err, claims)
	}
	if r := api.post("/auth/v1/signin/password", `{"email":"alice@example.com","password":"wrong-password"}`); r.Code != 400 ||
		r.Error.Code != "invalid_credentials" {
		t.Fatalf("a wrong password: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/password", `{"email":"nobody@example.com","password":"whatever-1234"}`); r.Code != 400 ||
		r.Error.Code != "invalid_credentials" {
		t.Fatalf("an unknown user: %d %s", r.Code, r.Body)
	}

	// ---- The done-when's RLS: per-user data through xb_auth.uid() ---------------
	aliceAPI := apiClient{t: t, ed: ed, ref: ref, key: pub, token: aliceSession.AccessToken}
	bobAPI := apiClient{t: t, ed: ed, ref: ref, key: pub, token: bobToken}
	anonAPI := apiClient{t: t, ed: ed, ref: ref, key: pub}
	for _, c := range []struct {
		cl    apiClient
		title string
	}{{aliceAPI, "alice's"}, {aliceAPI, "alice's too"}, {bobAPI, "bob's"}} {
		if r := c.cl.do("POST", "/data/v1/todos", fmt.Sprintf(`{"title":%q}`, c.title)); r.Code != 201 {
			t.Fatalf("insert %s: %d %s", c.title, r.Code, r.Body)
		}
	}
	if got := titles(aliceAPI.get("/data/v1/todos?select=title").rows(t), "title"); strings.Join(got, ",") != "alice's,alice's too" {
		t.Fatalf("alice sees %v", got)
	}
	if got := titles(bobAPI.get("/data/v1/todos?select=title").rows(t), "title"); strings.Join(got, ",") != "bob's" {
		t.Fatalf("bob sees %v", got)
	}
	if rows := anonAPI.get("/data/v1/todos").rows(t); len(rows) != 0 {
		t.Fatalf("anon sees %v", rows)
	}
	if r := bobAPI.do("POST", "/data/v1/todos", fmt.Sprintf(`{"title":"forged","owner_id":%q}`, alice)); r.Code != 403 {
		t.Fatalf("bob writes as alice: %d %s", r.Code, r.Body)
	}
	if rows := aliceAPI.get("/data/v1/my_profile").rows(t); len(rows) != 1 || rows[0]["email"] != "alice@example.com" {
		t.Fatalf("alice's profile view: %v", rows)
	}

	// ---- Refresh rotation and reuse detection -----------------------------------
	r = api.post("/auth/v1/token?grant_type=refresh_token", fmt.Sprintf(`{"refresh_token":%q}`, aliceSession.RefreshToken))
	if r.Code != 200 || r.Session.RefreshToken == "" || r.Session.RefreshToken == aliceSession.RefreshToken || r.Session.AccessToken == "" {
		t.Fatalf("refresh: %d %s", r.Code, r.Body)
	}
	rotated := r.Session
	if r := aliceAPI.get("/data/v1/todos"); r.Code != 200 {
		t.Fatalf("the first access token after a refresh: %d", r.Code)
	}
	// The old token again: theft, so the whole session ends …
	if r := api.post("/auth/v1/token?grant_type=refresh_token", fmt.Sprintf(`{"refresh_token":%q}`, aliceSession.RefreshToken)); r.Code != 400 ||
		r.Error.Code != "refresh_token_reused" {
		t.Fatalf("a reused refresh token: %d %s", r.Code, r.Body)
	}
	// … including the token the rotation handed out.
	if r := api.post("/auth/v1/token?grant_type=refresh_token", fmt.Sprintf(`{"refresh_token":%q}`, rotated.RefreshToken)); r.Code != 400 ||
		r.Error.Code != "invalid_refresh_token" {
		t.Fatalf("the family after reuse: %d %s", r.Code, r.Body)
	}
	if r := api.call("GET", "/auth/v1/user", "", rotated.AccessToken); r.Code != 401 || r.Error.Code != "session_not_found" {
		t.Fatalf("the user of an ended session: %d %s", r.Code, r.Body)
	}

	// ---- Sign out everywhere ---------------------------------------------------------
	s1 := api.post("/auth/v1/signin/password", `{"email":"alice@example.com","password":"alice-password-1"}`).Session
	s2 := api.post("/auth/v1/signin/password", `{"email":"alice@example.com","password":"alice-password-1"}`).Session
	if r := api.call("GET", "/auth/v1/user", "", s2.AccessToken); r.Code != 200 || r.Session.User.ID != uuid.Nil && !strings.Contains(r.Body, alice.String()) {
		t.Fatalf("the user: %d %s", r.Code, r.Body)
	}
	if r := api.call("POST", "/auth/v1/signout?scope=global", "", s1.AccessToken); r.Code != 204 {
		t.Fatalf("sign out everywhere: %d %s", r.Code, r.Body)
	}
	for _, s := range []authSession{s1, s2} {
		if r := api.post("/auth/v1/token?grant_type=refresh_token", fmt.Sprintf(`{"refresh_token":%q}`, s.RefreshToken)); r.Code != 400 {
			t.Fatalf("refresh after signing out everywhere: %d %s", r.Code, r.Body)
		}
	}

	// ---- Magic link and email code ---------------------------------------------------
	if r := api.post("/auth/v1/signin/otp", `{"email":"alice@example.com"}`); r.Code != 200 {
		t.Fatalf("magic link: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/otp", `{"email":"alice@example.com"}`); r.Code != 429 || r.Error.Code != "over_email_send_rate_limit" {
		t.Fatalf("a second code at once: %d %s", r.Code, r.Body)
	}
	code, _ = box.next(t, "alice@example.com")
	if r := api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"email","email":"alice@example.com","token":%q}`, code)); r.Code != 200 || r.Session.User.ID != alice {
		t.Fatalf("sign in by code: %d %s", r.Code, r.Body)
	}
	// An unknown address gets the same answer, and a new (unconfirmed) user.
	if r := api.post("/auth/v1/signin/otp", `{"email":"carol@example.com"}`); r.Code != 200 {
		t.Fatalf("magic link to a new address: %d %s", r.Code, r.Body)
	}
	code, _ = box.next(t, "carol@example.com")
	r = api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"magiclink","email":"carol@example.com","token":%q}`, code))
	if r.Code != 200 || r.Session.User.EmailConfirmedAt == nil {
		t.Fatalf("a new user by magic link: %d %s", r.Code, r.Body)
	}
	carol := r.Session
	// Someone signs up with another's address and leaves it unconfirmed; the
	// owner of the address signs in by code; the squatter's password is gone.
	if r := api.post("/auth/v1/signup", `{"email":"victim@example.com","password":"squatter-pass-1"}`); r.Code != 200 {
		t.Fatalf("squatter signup: %d %s", r.Code, r.Body)
	}
	box.next(t, "victim@example.com") // the confirmation, which the squatter never sees
	if r := api.post("/auth/v1/signin/otp", `{"email":"victim@example.com"}`); r.Code != 200 {
		t.Fatalf("victim magic link: %d %s", r.Code, r.Body)
	}
	code, _ = box.next(t, "victim@example.com")
	if r := api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"magiclink","email":"victim@example.com","token":%q}`, code)); r.Code != 200 {
		t.Fatalf("victim signs in: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/password", `{"email":"victim@example.com","password":"squatter-pass-1"}`); r.Code != 400 {
		t.Fatalf("the squatter's password after the owner confirmed: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signup", `{"email":"bad,addr@example.com","password":"whatever-pass-1"}`); r.Code != 400 || r.Error.Code != "invalid_email" {
		t.Fatalf("a comma in an address: %d %s", r.Code, r.Body)
	}

	// ---- Recovery, then a new password ----------------------------------------------
	if r := api.post("/auth/v1/recover", `{"email":"nobody@example.com"}`); r.Code != 200 {
		t.Fatalf("recover an unknown address: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/recover", `{"email":"bob@example.com"}`); r.Code != 200 {
		t.Fatalf("recover: %d %s", r.Code, r.Body)
	}
	code, _ = box.next(t, "bob@example.com")
	r = api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"recovery","email":"bob@example.com","token":%q}`, code))
	if r.Code != 200 {
		t.Fatalf("recovery code: %d %s", r.Code, r.Body)
	}
	if r := api.call("PATCH", "/auth/v1/user", `{"password":"bob-new-password-1","data":{"plan":"pro"}}`, r.Session.AccessToken); r.Code != 200 ||
		!strings.Contains(r.Body, `"plan":"pro"`) {
		t.Fatalf("a new password: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/password", `{"email":"bob@example.com","password":"bob-password-12"}`); r.Code != 400 {
		t.Fatalf("the old password: %d %s", r.Code, r.Body)
	}
	r = api.post("/auth/v1/signin/password", `{"email":"bob@example.com","password":"bob-new-password-1"}`)
	if r.Code != 200 {
		t.Fatalf("the new password: %d %s", r.Code, r.Body)
	}
	bob := r.Session

	// ---- Lockout after repeated failures --------------------------------------------------
	for i := range 5 {
		if r := api.post("/auth/v1/signin/password", `{"email":"carol@example.com","password":"not-carols-password"}`); r.Code != 400 {
			t.Fatalf("failure %d: %d %s", i, r.Code, r.Body)
		}
	}
	if r := api.post("/auth/v1/signin/password", `{"email":"carol@example.com","password":"not-carols-password"}`); r.Code != 429 ||
		r.Error.Code != "user_locked" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("locked: %d %s", r.Code, r.Body)
	}

	// ---- The admin API, with the secret key only ------------------------------------------
	if r := api.call("GET", "/auth/v1/admin/users", "", ""); r.Code != 403 || r.Error.Code != "admin_only" {
		t.Fatalf("admin with the publishable key: %d %s", r.Code, r.Body)
	}
	var list struct {
		Users []map[string]any `json:"users"`
		Total int              `json:"total"`
	}
	r = admin.call("GET", "/auth/v1/admin/users?q=example.com", "", "")
	if err := json.Unmarshal([]byte(r.Body), &list); err != nil || r.Code != 200 || list.Total != 4 || strings.Contains(r.Body, "argon2") {
		t.Fatalf("admin list: %d %s", r.Code, r.Body)
	}
	if r := admin.post("/auth/v1/admin/users", `{"email":"dave@example.com","password":"dave-password-1","email_confirm":true}`); r.Code != 201 {
		t.Fatalf("admin create: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/password", `{"email":"dave@example.com","password":"dave-password-1"}`); r.Code != 200 {
		t.Fatalf("an admin-made user signs in: %d %s", r.Code, r.Body)
	}
	if r := admin.call("PATCH", "/auth/v1/admin/users/"+bob.User.ID.String(), `{"ban_duration":"24h"}`, ""); r.Code != 200 ||
		!strings.Contains(r.Body, "banned_until") {
		t.Fatalf("ban: %d %s", r.Code, r.Body)
	}
	// A ban takes effect at the next refresh; the access token lasts its hour.
	if r := api.post("/auth/v1/token?grant_type=refresh_token", fmt.Sprintf(`{"refresh_token":%q}`, bob.RefreshToken)); r.Code != 403 ||
		r.Error.Code != "user_banned" {
		t.Fatalf("a banned user refreshes: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/password", `{"email":"bob@example.com","password":"bob-new-password-1"}`); r.Code != 403 {
		t.Fatalf("a banned user signs in: %d %s", r.Code, r.Body)
	}
	if r := (apiClient{t: t, ed: ed, ref: ref, key: pub, token: bob.AccessToken}).get("/data/v1/todos"); r.Code != 200 {
		t.Fatalf("a banned user's live access token: %d", r.Code)
	}
	// Deleting a user takes their rows with them (the owner's foreign key).
	if r := admin.call("DELETE", "/auth/v1/admin/users/"+bob.User.ID.String(), "", ""); r.Code != 204 {
		t.Fatalf("delete: %d %s", r.Code, r.Body)
	}
	var n int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM todos WHERE title = 'bob''s'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bob's todos after deleting bob: %d %v", n, err)
	}
	var gl struct {
		ActionLink string `json:"action_link"`
		EmailOTP   string `json:"email_otp"`
	}
	r = admin.post("/auth/v1/admin/generate-link", `{"type":"magiclink","email":"dave@example.com"}`)
	if err := json.Unmarshal([]byte(r.Body), &gl); err != nil || r.Code != 200 || !strings.Contains(gl.ActionLink, "/auth/v1/verify?") || len(gl.EmailOTP) != 6 {
		t.Fatalf("generate link: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"magiclink","email":"dave@example.com","token":%q}`, gl.EmailOTP)); r.Code != 200 {
		t.Fatalf("a generated link's code: %d %s", r.Code, r.Body)
	}

	// ---- JWKS and key rotation ------------------------------------------------------------
	oldKid := kidOf(t, aliceSession.AccessToken)
	var keys gen.SigningKeyList
	if code := e.Do("POST", base+"/auth/signing-keys/rotate", nil, &keys); code != 200 || len(keys.Items) != 2 {
		t.Fatalf("rotate: %d %+v", code, keys)
	}
	waitFor(t, 30*time.Second, "the new key reaches the edge", func() bool {
		s := api.post("/auth/v1/signin/password", `{"email":"dave@example.com","password":"dave-password-1"}`).Session
		return s.AccessToken != "" && kidOf(t, s.AccessToken) != oldKid
	})
	if ks := jwksOf(t, api); len(ks) != 2 || ks[oldKid] == nil {
		t.Fatalf("JWKS after rotation: %d keys", len(ks))
	}
	if r := (apiClient{t: t, ed: ed, ref: ref, key: pub, token: carol.AccessToken}).get("/data/v1/todos"); r.Code != 200 {
		t.Fatalf("a token signed with the old key: %d %s", r.Code, r.Body)
	}

	// ---- Monthly active users, the audit log and the dashboard ------------------------------
	ed.Flush(ctx)
	if code := e.Do("GET", base+"/auth/config", nil, &cfg); code != 200 || cfg.MonthlyActiveUsers < 4 || cfg.Email.Sent24h < 7 || cfg.Email.PlatformLeft != services.PlatformEmailsPerHour-7 {
		t.Fatalf("auth usage: %d mau=%d email=%+v", code, cfg.MonthlyActiveUsers, cfg.Email)
	}
	var users gen.AuthUserList
	if code := e.Do("GET", base+"/auth/users?q=alice", nil, &users); code != 200 || users.Total != 1 || users.Stats.Users != 4 {
		t.Fatalf("dashboard users: %d %+v", code, users)
	}
	var detail gen.AuthUserDetail
	if code := e.Do("GET", base+"/auth/users/"+alice.String(), nil, &detail); code != 200 || len(detail.Identities) != 1 {
		t.Fatalf("dashboard user: %d %+v", code, detail)
	}
	actions := map[string]bool{}
	for _, a := range detail.Audit {
		actions[a.Action] = true
	}
	for _, want := range []string{"user_signedup", "user_confirmed", "login", "token_reuse_detected", "logout"} {
		if !actions[want] {
			t.Errorf("alice's audit log lacks %s: %v", want, actions)
		}
	}
	var out gen.AuthSignOutResult
	if code := e.Do("POST", base+"/auth/users/"+alice.String()+"/signout", nil, &out); code != 200 || out.SessionsEnded < 1 {
		t.Fatalf("dashboard sign-out: %d %+v", code, out)
	}
	var invited gen.AuthUser
	if code := e.Do("POST", base+"/auth/users", gen.AuthUserCreate{Email: "erin@example.com", Invite: ptr(true)}, &invited); code != 201 || invited.InvitedAt == nil {
		t.Fatalf("invite: %d %+v", code, invited)
	}
	_, link = box.next(t, "erin@example.com")
	if lu, _ := url.Parse(link); lu == nil || lu.Query().Get("type") != "invite" {
		t.Fatalf("invitation link %q", link)
	}

	// ---- The platform's email allowance, then the project's own SMTP --------------------------
	var err429 error
	for i := range services.PlatformEmailsPerHour + 1 {
		if err429 = e.Services.QueueAuthMessage(ctx, edgeapi.AuthMessage{Ref: ref, Kind: edgeapi.EmailMagicLink,
			To: fmt.Sprintf("bulk%d@example.com", i), Code: "123456", Link: "https://x"}); err429 != nil {
			break
		}
	}
	if err429 == nil || !strings.Contains(err429.Error(), "rate limited") {
		t.Fatalf("the platform allowance: %v", err429)
	}
	if r := api.post("/auth/v1/signin/otp", `{"email":"frank@example.com"}`); r.Code != 429 || r.Error.Code != "over_email_send_rate_limit" {
		t.Fatalf("an email over the allowance: %d %s", r.Code, r.Body)
	}
	host, portS, _ := net.SplitHostPort(e.SMTP.Addr)
	port, _ := strconv.Atoi(portS)
	// The test's SMTP server is on loopback, which the outbound rules
	// refuse unless the platform admin allows the host.
	if code := e.Do("PUT", "/api/v1/admin/orgs/"+p.OrgID.String()+"/outbound", gen.OutboundAllowlist{Hosts: []string{host}}, nil); code != http.StatusOK {
		t.Fatalf("allow-list: %d", code)
	}
	tls := gen.AuthSMTPTls("none")
	if code := e.Do("PATCH", base+"/auth/config", gen.AuthConfigUpdate{Smtp: &gen.AuthSMTP{Host: host, Port: &port, From: "Auth App <auth@app.example.com>",
		Tls: &tls}, Templates: &map[string]gen.AuthEmailTemplate{"magic_link": {Subject: "Your Auth App code", Body: "Hi! Your code: {{.Code}}\n{{.Link}}\n"}}},
		&cfg); code != 200 || cfg.Smtp == nil || !cfg.Email.OwnSmtp {
		t.Fatalf("own SMTP: %d %+v", code, cfg.Smtp)
	}
	if r := api.post("/auth/v1/signin/otp", `{"email":"frank@example.com"}`); r.Code != 200 {
		t.Fatalf("an email through the project's SMTP: %d %s", r.Code, r.Body)
	}
	if code, _ := box.next(t, "frank@example.com"); code == "" {
		t.Fatal("no code in the custom template's email")
	}
	var fromApp bool
	for _, m := range e.SMTP.Mail() {
		if len(m.To) > 0 && m.To[0] == "frank@example.com" {
			fromApp = strings.Contains(m.Data, "Your Auth App code") && strings.Contains(m.From, "auth@app.example.com")
		}
	}
	if !fromApp {
		t.Fatal("the email didn't use the project's sender and template")
	}
}

type ecdsaPub = ecdsa.PublicKey

var base64URL = base64.RawURLEncoding

func jwksOf(t *testing.T, a authAPI) map[string]*ecdsaPub {
	t.Helper()
	r := authAPI{t: t, ed: a.ed, ref: a.ref}.call("GET", "/auth/v1/.well-known/jwks.json", "", "")
	var set struct {
		Keys []jwtes.JWK `json:"keys"`
	}
	if r.Code != 200 || json.Unmarshal([]byte(r.Body), &set) != nil {
		t.Fatalf("jwks: %d %s", r.Code, r.Body)
	}
	out := map[string]*ecdsaPub{}
	for _, k := range set.Keys {
		pub, err := k.Public()
		if err != nil {
			t.Fatal(err)
		}
		out[k.Kid] = pub
	}
	return out
}

func kidOf(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", token)
	}
	var h struct {
		Kid string `json:"kid"`
	}
	b, _ := base64URL.DecodeString(parts[0])
	_ = json.Unmarshal(b, &h)
	return h.Kid
}
