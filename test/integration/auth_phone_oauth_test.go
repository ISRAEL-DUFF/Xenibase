package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/edge"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/jwtes"
	"github.com/israel-duff/xenibase/internal/outbound"
	"github.com/israel-duff/xenibase/internal/services"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/totp"
	"github.com/israel-duff/xenibase/test/testenv"
)

// fakeGoogle is an OAuth provider that signs in one user for the code
// "good-code".
func fakeGoogle(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("code") != "good-code" || r.Form.Get("client_id") != "google-client" || r.Form.Get("client_secret") != "google-secret" ||
				r.Form.Get("code_verifier") == "" || !strings.HasSuffix(r.Form.Get("redirect_uri"), "/auth/v1/callback") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"google-at","token_type":"Bearer"}`))
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer google-at" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"sub":"g-1001","email":"ada@example.com","email_verified":true,"name":"Ada L","picture":"https://img.example.com/ada.png"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sentTo counts the codes phone was sent by channel.
func sentTo(e *testenv.Env, channel, phone string) int {
	n := 0
	for _, m := range e.Phone.Sent() {
		if m.Channel == channel && m.To == strings.TrimPrefix(phone, "+") {
			n++
		}
	}
	return n
}

// phoneCode sends what is queued and waits for a code to phone after the
// first after.
func phoneCode(t *testing.T, e *testenv.Env, channel, phone string, after int) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := e.Services.SendAuthEmails(context.Background()); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range e.Phone.Sent() {
			if m.Channel != channel || m.To != strings.TrimPrefix(phone, "+") {
				continue
			}
			if n++; n > after {
				if m.Code != "" {
					return m.Code
				}
				for _, f := range strings.Fields(m.Body) {
					if len(f) == 6 && strings.Trim(f, "0123456789") == "" {
						return f
					}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	var dump []string
	rows, _ := e.DB.Query(context.Background(), `SELECT channel, via, attempts, coalesce(last_error, ''), sent_at IS NOT NULL FROM auth_message_outbox`)
	for rows != nil && rows.Next() {
		var ch, via, le string
		var n int
		var sent bool
		_ = rows.Scan(&ch, &via, &n, &le, &sent)
		dump = append(dump, fmt.Sprintf("%s/%s tries=%d sent=%v %s", ch, via, n, sent, le))
	}
	t.Fatalf("no %s code reached %s; outbox: %v; fake got %+v", channel, phone, dump, e.Phone.Sent())
	return ""
}

// TestAuthPhoneOAuthMFA is V4-M32's done-when (V4 §4): sign-in with a
// WhatsApp code and with Google, a custom-claims hook adding org_id that
// row-level security uses, and an SMS-pumping run stopped by the country
// list, the per-number limit and the daily cap. Around it: SMS codes and
// phone sign-up, anonymous users upgraded by a phone, linking by verified
// email (without a squatter's password), identities, TOTP and the
// required-MFA policy, the before-sign-up Postgres hook, the after-sign-up
// webhook, captcha, and per-message metering.
func TestAuthPhoneOAuthMFA(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	creds := e.CreateProject("phone-app")
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
	var pub string
	for _, k := range en.Keys {
		if k.Key.Kind == gen.ApiKeyKindPublishable {
			pub = k.Value
		}
	}
	ref := *en.Services.Ref
	base := "/api/v1/projects/" + pid.String()
	hookRole := store.AuthHookRole(p.DbName)

	// The app: members carry an org, documents belong to orgs, and the
	// custom-claims hook puts the user's org in their token.
	app := e.MustConnect(creds.Connection.PooledUrl)
	defer app.Close(ctx)
	for _, stmt := range []string{
		`CREATE TABLE members (user_id uuid PRIMARY KEY REFERENCES xb_auth.users (id) ON DELETE CASCADE, org_id text NOT NULL)`,
		fmt.Sprintf(`GRANT SELECT ON members TO %q`, hookRole),
		`CREATE FUNCTION public.add_claims(event jsonb) RETURNS jsonb LANGUAGE sql STABLE AS $$
		   SELECT jsonb_build_object('claims', (event -> 'claims') || coalesce(
		     (SELECT jsonb_build_object('org_id', org_id) FROM members WHERE user_id = (event ->> 'user_id')::uuid), '{}'::jsonb)) $$`,
		`CREATE FUNCTION public.check_signup(event jsonb) RETURNS jsonb LANGUAGE sql STABLE AS $$
		   SELECT CASE WHEN event -> 'user' ->> 'email' LIKE '%@blocked.example'
		     THEN '{"decision":"reject","message":"this domain can''t sign up"}'::jsonb ELSE '{}'::jsonb END $$`,
		`CREATE TABLE docs (id bigserial PRIMARY KEY, org_id text NOT NULL, title text NOT NULL)`,
		`ALTER TABLE docs ENABLE ROW LEVEL SECURITY`,
		fmt.Sprintf(`CREATE POLICY same_org ON docs FOR SELECT TO %q USING (org_id = xb_auth.claim('org_id'))`, store.UserRole(p.DbName)),
		`INSERT INTO docs (org_id, title) VALUES ('acme', 'acme plan'), ('acme', 'acme budget'), ('globex', 'globex secrets')`,
	} {
		if _, err := app.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// The hook role reads only what it was granted.
	if _, err := app.Exec(ctx, fmt.Sprintf(`SET ROLE %q`, hookRole)); err == nil {
		t.Fatal("the owner could become the hook role")
	}

	rc := newReceiver(t)
	allowLocal(t, e, e.OrgID)
	google := fakeGoogle(t)
	var cfg gen.AuthConfig
	if code := e.Do("PATCH", base+"/auth/config", gen.AuthConfigUpdate{
		Settings: &gen.AuthSettings{
			SiteUrl: ptr("https://app.example.com"), RedirectUrls: &[]string{"https://app.example.com/callback"},
			PhoneChannels:    &[]gen.AuthSettingsPhoneChannels{gen.AuthSettingsPhoneChannelsSms, gen.AuthSettingsPhoneChannelsWhatsapp},
			AnonymousEnabled: ptr(true), MfaPhone: ptr(true),
			Oauth:            &map[string]gen.AuthOAuthSetting{"google": {Enabled: true, ClientId: "google-client"}},
			CustomClaimsHook: ptr("public.add_claims"), BeforeSignupHook: ptr("public.check_signup"),
			AfterSignupUrl: ptr(rc.URL + "/after-signup"),
		},
		OauthSecrets: &map[string]gen.AuthOAuthSecret{"google": {ClientSecret: ptr("google-secret")}},
	}, &cfg); code != 200 {
		t.Fatalf("auth config: %d", code)
	}
	if cfg.HookSecret == nil || !strings.HasPrefix(*cfg.HookSecret, "whsec_") || !(*cfg.OauthSecretSet)["google"] ||
		*cfg.HookRole != hookRole || !strings.HasSuffix(*cfg.OauthCallbackUrl, "/auth/v1/callback") || !cfg.Phone.PlatformWhatsapp {
		t.Fatalf("auth config: %+v", cfg)
	}
	// A developer reads the settings but not the hook secret.
	if code := e.Do("POST", base+"/members", map[string]any{"email": "dev@example.com", "role": "developer"}, nil); code != http.StatusOK {
		t.Fatalf("invite a developer: %d", code)
	}
	dev := e.AcceptInvitation(e.MailToken("dev@example.com", "invitation"), "dev@example.com")
	var devCfg gen.AuthConfig
	if code := dev.Do("GET", base+"/auth/config", nil, &devCfg); code != http.StatusOK || devCfg.HookSecret != nil ||
		devCfg.HookSecretSet == nil || !*devCfg.HookSecretSet {
		t.Fatalf("a developer's auth config: %d %v %v", code, devCfg.HookSecret, devCfg.HookSecretSet)
	}
	// An Apple app needs its key ids; a hook must name a function.
	if code := e.Do("PATCH", base+"/auth/config", gen.AuthConfigUpdate{Settings: &gen.AuthSettings{
		Oauth: &map[string]gen.AuthOAuthSetting{"apple": {Enabled: true, ClientId: "com.example.app"}}}}, nil); code != 400 {
		t.Fatalf("apple without its key: %d", code)
	}
	if code := e.Do("PATCH", base+"/auth/config", gen.AuthConfigUpdate{Settings: &gen.AuthSettings{
		CustomClaimsHook: ptr("drop table x;")}}, nil); code != 400 {
		t.Fatalf("a hook that isn't a function name: %d", code)
	}

	ed := e.StartEdge(func(c *edge.Config) {
		c.OAuthEndpoints = map[string]edge.OAuthEndpoints{"google": {AuthURL: google.URL + "/auth", TokenURL: google.URL + "/token",
			UserURL: google.URL + "/userinfo"}}
	})
	api := authAPI{t: t, ed: ed, ref: ref, key: pub}
	keyless := authAPI{t: t, ed: ed, ref: ref}
	waitFor(t, 30*time.Second, "the edge reaches the project's database", func() bool {
		return api.call("GET", "/data/v1/health", "", "").Code == 200
	})
	waitFor(t, 30*time.Second, "the auth settings reach the edge", func() bool {
		r := api.call("GET", "/auth/v1/settings", "", "")
		return strings.Contains(r.Body, `"google":true`) && strings.Contains(r.Body, `"whatsapp"`)
	})
	jwks := jwksOf(t, api)

	// ---- Sign-in with a WhatsApp code ---------------------------------------------
	const ngozi = "+2348031234567"
	before := sentTo(e, "whatsapp", ngozi)
	if r := api.post("/auth/v1/signin/otp", `{"phone":"0803 123 4567","channel":"whatsapp"}`); r.Code != 200 {
		t.Fatalf("whatsapp otp: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/otp", `{"phone":"08031234567","channel":"whatsapp"}`); r.Code != 429 || r.Error.Code != "over_sms_send_rate_limit" {
		t.Fatalf("a second code at once: %d %s", r.Code, r.Body)
	}
	code := phoneCode(t, e, "whatsapp", ngozi, before)
	if r := api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"sms","phone":%q,"token":"000000"}`, ngozi)); r.Code != 403 {
		t.Fatalf("a wrong code: %d %s", r.Code, r.Body)
	}
	r := api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"sms","phone":"08031234567","token":%q}`, code))
	if r.Code != 200 || r.Session.AccessToken == "" || !strings.Contains(r.Body, `"phone":"+2348031234567"`) ||
		strings.Contains(r.Body, `"phone_confirmed_at":null`) {
		t.Fatalf("verify whatsapp: %d %s", r.Code, r.Body)
	}
	ngoziID, ngoziSession := r.Session.User.ID, r.Session
	// The platform's WhatsApp is metered per message, with its cost.
	var waSends, waCost int64
	if err := e.DB.QueryRow(ctx, `SELECT count(*), coalesce(sum(cost_minor), 0) FROM message_sends
		WHERE project_id = $1 AND channel = 'whatsapp' AND status = 'sent'`, pid).Scan(&waSends, &waCost); err != nil || waSends != 1 || waCost != 1500 {
		t.Fatalf("whatsapp sends: %d cost %d (%v)", waSends, waCost, err)
	}
	var metered float64
	if err := e.DB.QueryRow(ctx, `SELECT coalesce(sum(quantity), 0)::float8 FROM usage_records WHERE project_id = $1 AND metric = $2`,
		pid, "messages_whatsapp").Scan(&metered); err != nil || metered != 1 {
		t.Fatalf("whatsapp metered: %v (%v)", metered, err)
	}
	// The after-sign-up webhook, signed with the hook secret.
	got := func() []hookReq {
		var out []hookReq
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) && len(out) == 0 {
			if err := e.Services.SendAuthEmails(ctx); err != nil {
				t.Fatal(err)
			}
			out = rc.received()
			time.Sleep(100 * time.Millisecond)
		}
		return out
	}()
	if len(got) == 0 {
		t.Fatal("no after-sign-up webhook")
	}
	var ev struct {
		Type string `json:"type"`
		Data struct {
			User struct {
				ID    string `json:"id"`
				Phone string `json:"phone"`
			} `json:"user"`
			Method string `json:"method"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got[0].Body, &ev); err != nil || ev.Type != "after_signup" || ev.Data.User.ID != ngoziID.String() ||
		ev.Data.User.Phone != ngozi {
		t.Fatalf("after-sign-up event: %s (%v)", got[0].Body, err)
	}
	if err := outbound.Verify(*cfg.HookSecret, got[0].Header.Get("Xenibase-Signature"), got[0].Body, time.Now(), 5*time.Minute); err != nil {
		t.Fatalf("hook signature: %v", err)
	}

	// ---- The custom-claims hook's org_id, used by row-level security --------------
	if _, err := app.Exec(ctx, `INSERT INTO members (user_id, org_id) VALUES ($1, 'acme')`, ngoziID); err != nil {
		t.Fatal(err)
	}
	r = api.post("/auth/v1/token?grant_type=refresh_token", fmt.Sprintf(`{"refresh_token":%q}`, ngoziSession.RefreshToken))
	if r.Code != 200 {
		t.Fatalf("refresh: %d %s", r.Code, r.Body)
	}
	ngoziSession = r.Session
	claims, err := jwtes.Verify(ngoziSession.AccessToken, jwks, ref, time.Now())
	if err != nil || claims["org_id"] != "acme" || claims["sub"] != ngoziID.String() || claims["role"] != "user" || claims["phone"] != ngozi {
		t.Fatalf("claims: %v %v", claims, err)
	}
	docs := func(token string) []string {
		t.Helper()
		r := api.call("GET", "/data/v1/docs?select=title&order=title", "", token)
		if r.Code != 200 {
			t.Fatalf("docs: %d %s", r.Code, r.Body)
		}
		var res struct {
			Data []struct {
				Title string `json:"title"`
			} `json:"data"`
		}
		_ = json.Unmarshal([]byte(r.Body), &res)
		out := []string{}
		for _, x := range res.Data {
			out = append(out, x.Title)
		}
		return out
	}
	if got := docs(ngoziSession.AccessToken); strings.Join(got, ",") != "acme budget,acme plan" {
		t.Fatalf("acme's documents: %v", got)
	}

	// ---- Sign-in with Google (PKCE), linked by verified email ---------------------
	// Someone signed up with Ada's address first and never confirmed it.
	if r := api.post("/auth/v1/signup", `{"email":"ada@example.com","password":"squatter-password"}`); r.Code != 200 {
		t.Fatalf("squatter: %d %s", r.Code, r.Body)
	}
	vb := make([]byte, 32)
	_, _ = rand.Read(vb)
	verifier := base64.RawURLEncoding.EncodeToString(vb)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	q := url.Values{"provider": {"google"}, "redirect_to": {"https://app.example.com/callback"}, "code_challenge": {challenge},
		"code_challenge_method": {"s256"}}
	if r := keyless.call("GET", "/auth/v1/authorize?provider=github&redirect_to=https://app.example.com/callback", "", ""); r.Code != 400 {
		t.Fatalf("a provider not turned on: %d %s", r.Code, r.Body)
	}
	if r := keyless.call("GET", "/auth/v1/authorize?provider=google&redirect_to=https://evil.example.net/", "", ""); r.Code != 400 {
		t.Fatalf("a redirect not allowed: %d %s", r.Code, r.Body)
	}
	r = keyless.call("GET", "/auth/v1/authorize?"+q.Encode(), "", "")
	loc, _ := url.Parse(r.Header.Get("Location"))
	if r.Code != http.StatusFound || loc == nil || !strings.HasPrefix(loc.String(), google.URL+"/auth?") ||
		loc.Query().Get("client_id") != "google-client" || loc.Query().Get("code_challenge") == "" || loc.Query().Get("state") == "" {
		t.Fatalf("authorize: %d %s", r.Code, r.Header.Get("Location"))
	}
	state := loc.Query().Get("state")
	r = keyless.call("GET", "/auth/v1/callback?"+url.Values{"code": {"good-code"}, "state": {state}}.Encode(), "", "")
	back, _ := url.Parse(r.Header.Get("Location"))
	if r.Code != http.StatusSeeOther || back == nil || back.Host != "app.example.com" || back.Query().Get("code") == "" {
		t.Fatalf("callback: %d %s %s", r.Code, r.Header.Get("Location"), r.Body)
	}
	authCode := back.Query().Get("code")
	if r := keyless.call("GET", "/auth/v1/callback?"+url.Values{"code": {"good-code"}, "state": {state}}.Encode(), "", ""); r.Code != 400 {
		t.Fatalf("a state used twice: %d", r.Code)
	}
	if r := api.post("/auth/v1/token?grant_type=pkce", fmt.Sprintf(`{"auth_code":%q,"code_verifier":"wrong-verifier-wrong-verifier-wrong-verifier"}`, authCode)); r.Code != 403 {
		t.Fatalf("a wrong verifier: %d %s", r.Code, r.Body)
	}
	// A wrong verifier used the code up: start again.
	r = keyless.call("GET", "/auth/v1/authorize?"+q.Encode(), "", "")
	loc, _ = url.Parse(r.Header.Get("Location"))
	r = keyless.call("GET", "/auth/v1/callback?"+url.Values{"code": {"good-code"}, "state": {loc.Query().Get("state")}}.Encode(), "", "")
	back, _ = url.Parse(r.Header.Get("Location"))
	r = api.post("/auth/v1/token?grant_type=pkce", fmt.Sprintf(`{"auth_code":%q,"code_verifier":%q}`, back.Query().Get("code"), verifier))
	if r.Code != 200 || r.Session.User.Email != "ada@example.com" || r.Session.User.EmailConfirmedAt == nil {
		t.Fatalf("pkce: %d %s", r.Code, r.Body)
	}
	adaSession := r.Session
	if r := api.post("/auth/v1/token?grant_type=pkce", fmt.Sprintf(`{"auth_code":%q,"code_verifier":%q}`, back.Query().Get("code"), verifier)); r.Code != 404 {
		t.Fatalf("a code used twice: %d %s", r.Code, r.Body)
	}
	// The squatter's password didn't come along.
	if r := api.post("/auth/v1/signin/password", `{"email":"ada@example.com","password":"squatter-password"}`); r.Code != 400 {
		t.Fatalf("the squatter's password: %d %s", r.Code, r.Body)
	}
	r = api.call("GET", "/auth/v1/user", "", adaSession.AccessToken)
	var adaUser struct {
		Identities []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"identities"`
		UserMetadata map[string]any `json:"user_metadata"`
	}
	_ = json.Unmarshal([]byte(r.Body), &adaUser)
	var providers []string
	for _, i := range adaUser.Identities {
		providers = append(providers, i.Provider)
	}
	sort.Strings(providers)
	if r.Code != 200 || strings.Join(providers, ",") != "email,google" {
		t.Fatalf("ada's identities: %d %s", r.Code, r.Body)
	}
	// Signing in with Google again finds the same user.
	r = keyless.call("GET", "/auth/v1/authorize?provider=google&redirect_to=https://app.example.com/callback", "", "")
	loc, _ = url.Parse(r.Header.Get("Location"))
	r = keyless.call("GET", "/auth/v1/callback?"+url.Values{"code": {"good-code"}, "state": {loc.Query().Get("state")}}.Encode(), "", "")
	back, _ = url.Parse(r.Header.Get("Location"))
	frag, _ := url.ParseQuery(back.Fragment)
	if implicit, err := jwtes.Verify(frag.Get("access_token"), jwks, ref, time.Now()); err != nil || implicit["email"] != "ada@example.com" {
		t.Fatalf("implicit sign-in: %s (%v)", r.Header.Get("Location"), err)
	}

	// ---- The before-sign-up hook --------------------------------------------------
	if r := api.post("/auth/v1/signup", `{"email":"mallory@blocked.example","password":"mallory-password"}`); r.Code != 403 ||
		r.Error.Code != "signup_rejected" || !strings.Contains(r.Body, "can't sign up") {
		t.Fatalf("a blocked domain: %d %s", r.Code, r.Body)
	}

	// ---- Phone sign-up by SMS, and an anonymous user who adds a phone -------------
	const tunde = "+2348051112222"
	before = sentTo(e, "sms", tunde)
	if r := api.post("/auth/v1/signup", `{"phone":"08051112222","password":"tunde-password-1"}`); r.Code != 200 || !strings.Contains(r.Body, "confirmation_sent") {
		t.Fatalf("phone sign-up: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/password", fmt.Sprintf(`{"phone":%q,"password":"tunde-password-1"}`, tunde)); r.Code != 403 ||
		r.Error.Code != "phone_not_confirmed" {
		t.Fatalf("before confirming: %d %s", r.Code, r.Body)
	}
	code = phoneCode(t, e, "sms", tunde, before)
	if r := api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"sms","phone":%q,"token":%q}`, tunde, code)); r.Code != 200 {
		t.Fatalf("confirm phone: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/password", `{"phone":"08051112222","password":"tunde-password-1"}`); r.Code != 200 {
		t.Fatalf("phone and password: %d %s", r.Code, r.Body)
	}
	r = api.post("/auth/v1/signup", `{}`)
	if r.Code != 200 || !strings.Contains(r.Body, `"is_anonymous":true`) {
		t.Fatalf("anonymous: %d %s", r.Code, r.Body)
	}
	anon := r.Session
	if c, _ := jwtes.Verify(anon.AccessToken, jwks, ref, time.Now()); c["is_anonymous"] != true {
		t.Fatalf("anonymous claims: %v", c)
	}
	const kemi = "+2348069998888"
	before = sentTo(e, "sms", kemi)
	if r := api.call("PATCH", "/auth/v1/user", `{"phone":"08069998888"}`, anon.AccessToken); r.Code != 200 {
		t.Fatalf("add a phone: %d %s", r.Code, r.Body)
	}
	code = phoneCode(t, e, "sms", kemi, before)
	r = api.post("/auth/v1/verify", fmt.Sprintf(`{"type":"phone_change","phone":%q,"token":%q}`, kemi, code))
	if r.Code != 200 || r.Session.User.ID != anon.User.ID || !strings.Contains(r.Body, `"is_anonymous":false`) {
		t.Fatalf("upgrade: %d %s", r.Code, r.Body)
	}
	kemiSession := r.Session
	// Its only identity can't be unlinked.
	r = api.call("GET", "/auth/v1/user", "", kemiSession.AccessToken)
	var kemiUser struct {
		Identities []struct {
			ID string `json:"id"`
		} `json:"identities"`
	}
	_ = json.Unmarshal([]byte(r.Body), &kemiUser)
	if len(kemiUser.Identities) != 1 {
		t.Fatalf("kemi's identities: %s", r.Body)
	}
	if r := api.call("DELETE", "/auth/v1/user/identities/"+kemiUser.Identities[0].ID, "", kemiSession.AccessToken); r.Code != 422 {
		t.Fatalf("unlink the last identity: %d %s", r.Code, r.Body)
	}

	// ---- TOTP, and the required-MFA policy ----------------------------------------
	r = api.call("POST", "/auth/v1/factors", `{"factor_type":"totp","friendly_name":"phone app"}`, ngoziSession.AccessToken)
	var enrolled struct {
		ID   string `json:"id"`
		TOTP struct {
			Secret string `json:"secret"`
			URI    string `json:"uri"`
		} `json:"totp"`
	}
	if err := json.Unmarshal([]byte(r.Body), &enrolled); err != nil || r.Code != 200 || enrolled.TOTP.Secret == "" ||
		!strings.HasPrefix(enrolled.TOTP.URI, "otpauth://totp/") {
		t.Fatalf("enroll: %d %s", r.Code, r.Body)
	}
	r = api.call("POST", "/auth/v1/factors/"+enrolled.ID+"/challenge", `{}`, ngoziSession.AccessToken)
	var ch struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(r.Body), &ch)
	if r.Code != 200 || ch.ID == "" {
		t.Fatalf("challenge: %d %s", r.Code, r.Body)
	}
	if r := api.call("POST", "/auth/v1/factors/"+enrolled.ID+"/verify", fmt.Sprintf(`{"challenge_id":%q,"code":"000000"}`, ch.ID),
		ngoziSession.AccessToken); r.Code != 422 {
		t.Fatalf("a wrong TOTP code: %d %s", r.Code, r.Body)
	}
	totpCode, _ := totp.Code(enrolled.TOTP.Secret, time.Now())
	r = api.call("POST", "/auth/v1/factors/"+enrolled.ID+"/verify", fmt.Sprintf(`{"challenge_id":%q,"code":%q}`, ch.ID, totpCode),
		ngoziSession.AccessToken)
	if r.Code != 200 {
		t.Fatalf("verify totp: %d %s", r.Code, r.Body)
	}
	ngoziAAL2 := r.Session
	if c, _ := jwtes.Verify(ngoziAAL2.AccessToken, jwks, ref, time.Now()); c["aal"] != "aal2" || c["org_id"] != "acme" {
		t.Fatalf("aal2 claims: %v", c)
	}
	if code := e.Do("PATCH", base+"/auth/config", gen.AuthConfigUpdate{Settings: &gen.AuthSettings{
		MfaPolicy: ptr(gen.AuthSettingsMfaPolicyRequired)}}, nil); code != 200 {
		t.Fatalf("require mfa: %d", code)
	}
	waitFor(t, 30*time.Second, "the MFA policy reaches the edge", func() bool {
		return strings.Contains(api.call("GET", "/auth/v1/settings", "", "").Body, `"mfa":"required"`)
	})
	if r := api.call("GET", "/data/v1/docs", "", adaSession.AccessToken); r.Code != 403 || !strings.Contains(r.Body, "mfa_required") {
		t.Fatalf("aal1 under required MFA: %d %s", r.Code, r.Body)
	}
	if got := docs(ngoziAAL2.AccessToken); len(got) != 2 {
		t.Fatalf("aal2 under required MFA: %v", got)
	}
	// Removing the factor needs aal2, and drops the sessions back.
	if r := api.call("DELETE", "/auth/v1/factors/"+enrolled.ID, "", ngoziSession.AccessToken); r.Code != 403 {
		t.Fatalf("unenroll at aal1: %d %s", r.Code, r.Body)
	}
	if r := api.call("DELETE", "/auth/v1/factors/"+enrolled.ID, "", ngoziAAL2.AccessToken); r.Code != 200 {
		t.Fatalf("unenroll: %d %s", r.Code, r.Body)
	}
	if code := e.Do("PATCH", base+"/auth/config", gen.AuthConfigUpdate{Settings: &gen.AuthSettings{MfaPolicy: ptr(gen.AuthSettingsMfaPolicyOptional)},
		CaptchaSecret: ptr("turnstile-secret")}, nil); code != 200 {
		t.Fatalf("optional mfa: %d", code)
	}
	if code := e.Do("PATCH", base+"/auth/config", gen.AuthConfigUpdate{Settings: &gen.AuthSettings{CaptchaEnabled: ptr(true),
		CaptchaSiteKey: ptr("site-key")}}, nil); code != 200 {
		t.Fatalf("captcha: %d", code)
	}

	// ---- Captcha ------------------------------------------------------------------
	waitFor(t, 30*time.Second, "captcha reaches the edge", func() bool {
		return strings.Contains(api.call("GET", "/auth/v1/settings", "", "").Body, `"captcha":true`)
	})
	if r := api.post("/auth/v1/signin/otp", `{"phone":"08070000001"}`); r.Code != 400 || r.Error.Code != "captcha_failed" {
		t.Fatalf("no captcha: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/otp", `{"phone":"08070000001","gotrue_meta_security":{"captcha_token":"fail"}}`); r.Code != 400 {
		t.Fatalf("a failed captcha: %d %s", r.Code, r.Body)
	}
	if r := api.post("/auth/v1/signin/otp", `{"phone":"08070000001","gotrue_meta_security":{"captcha_token":"pass"}}`); r.Code != 200 {
		t.Fatalf("a passed captcha: %d %s", r.Code, r.Body)
	}

	// ---- SMS pumping: the country list, the per-number limit, the daily cap -------
	if r := api.post("/auth/v1/signin/otp", `{"phone":"+447700900123","gotrue_meta_security":{"captcha_token":"pass"}}`); r.Code != 403 ||
		r.Error.Code != "phone_country_not_allowed" {
		t.Fatalf("a UK number: %d %s", r.Code, r.Body)
	}
	pump := func(to string) error {
		return e.Services.QueueAuthMessage(ctx, edgeapi.AuthMessage{Ref: ref, Channel: edgeapi.ChannelSMS, Kind: edgeapi.PhoneCode, To: to, Code: "123456"})
	}
	if err := pump("+447700900123"); !errors.Is(err, services.ErrNotAllowed) {
		t.Fatalf("the server's country check: %v", err)
	}
	var perNumber error
	for i := range services.PerNumberPerHour + 2 {
		if perNumber = pump("+2348090000001"); perNumber != nil {
			if i < services.PerNumberPerHour {
				t.Fatalf("refused after %d: %v", i, perNumber)
			}
			break
		}
	}
	var le *services.LimitError
	if !errors.As(perNumber, &le) || le.Limit != "number" {
		t.Fatalf("the per-number limit: %v", perNumber)
	}
	if code := e.Do("PATCH", base+"/auth/config", gen.AuthConfigUpdate{Settings: &gen.AuthSettings{PhoneDailyCap: ptr(20)}}, nil); code != 200 {
		t.Fatalf("daily cap: %d", code)
	}
	accepted, refusedAt := 0, 0
	for i := range 40 {
		err := pump(fmt.Sprintf("+23480910000%02d", i))
		if err == nil {
			accepted++
			continue
		}
		if !errors.As(err, &le) || le.Limit != "daily" {
			t.Fatalf("pump %d: %v", i, err)
		}
		refusedAt = i
		break
	}
	var today int64
	if err := e.DB.QueryRow(ctx, `SELECT count(*) FROM auth_message_outbox WHERE project_id = $1 AND channel <> 'email'`, pid).Scan(&today); err != nil {
		t.Fatal(err)
	}
	if refusedAt == 0 || today != 20 {
		t.Fatalf("the daily cap: %d accepted, refused at %d, %d queued today", accepted, refusedAt, today)
	}
	var alerts []string
	rows, err := e.DB.Query(ctx, `SELECT kind FROM auth_alerts WHERE project_id = $1 ORDER BY kind`, pid)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		alerts = append(alerts, k)
	}
	rows.Close()
	if strings.Join(alerts, ",") != "phone_daily_cap,phone_spike" {
		t.Fatalf("alerts: %v", alerts)
	}
	// Through the edge, the cap is a 429 the app can show.
	waitFor(t, 30*time.Second, "the cap is reached through the edge", func() bool {
		r := api.post("/auth/v1/signin/otp", `{"phone":"08099999999","gotrue_meta_security":{"captcha_token":"pass"}}`)
		return r.Code == 429 && r.Error.Code == "over_sms_send_rate_limit" && strings.Contains(r.Body, "daily limit")
	})
	// Free projects bring their own provider where the platform says so.
	e.Services.Phone.DisallowFreePlans = true
	err = pump("+2348011111111")
	e.Services.Phone.DisallowFreePlans = false
	if !errors.Is(err, services.ErrNotAllowed) {
		t.Fatalf("a Free project on the platform's SMS: %v", err)
	}

	// The dashboard shows the month's spend and the hook deliveries.
	if code := e.Do("GET", base+"/auth/config", nil, &cfg); code != 200 || cfg.Phone.DailyCap != 20 || len(cfg.Phone.Month) == 0 {
		t.Fatalf("spend: %d %+v", code, cfg.Phone)
	}
	var hooks gen.AuthHookDeliveryList
	if code := e.Do("GET", base+"/auth/hooks", nil, &hooks); code != 200 || len(hooks.Items) == 0 || hooks.Items[len(hooks.Items)-1].DeliveredAt == nil {
		t.Fatalf("hook deliveries: %d %+v", code, hooks.Items)
	}

	// ---- Tenant SQL can't step out of its role --------------------------------------
	// A function that resets the role lands on the same login: it can't read
	// the auth tables or become the service role (which bypasses RLS).
	svcRole := store.ServiceRole(p.DbName)
	for _, stmt := range []string{
		`CREATE FUNCTION public.whoami() RETURNS text LANGUAGE plpgsql VOLATILE AS $$
		   BEGIN RESET ROLE; RETURN session_user || '/' || current_user; END $$`,
		`CREATE FUNCTION public.escape_auth() RETURNS text LANGUAGE plpgsql VOLATILE AS $$
		   BEGIN RESET ROLE; RETURN (SELECT encrypted_password FROM xb_auth.users WHERE encrypted_password IS NOT NULL LIMIT 1); END $$`,
		fmt.Sprintf(`CREATE FUNCTION public.escape_service() RETURNS bigint LANGUAGE plpgsql VOLATILE AS $$
		   BEGIN RESET ROLE; SET ROLE %q; RETURN (SELECT count(*) FROM docs); END $$`, svcRole),
	} {
		if _, err := app.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	waitFor(t, 30*time.Second, "the new functions reach the edge", func() bool {
		return api.call("POST", "/data/v1/rpc/whoami", `{}`, "").Code == 200
	})
	r = api.call("POST", "/data/v1/rpc/whoami", `{}`, "")
	anonRole := store.AnonRole(p.DbName)
	if !strings.Contains(r.Body, anonRole+"/"+anonRole) {
		t.Fatalf("RESET ROLE as anon: %d %s", r.Code, r.Body)
	}
	if r := api.call("POST", "/data/v1/rpc/whoami", `{}`, ngoziAAL2.AccessToken); !strings.Contains(r.Body, store.UserRole(p.DbName)+"/") {
		t.Fatalf("RESET ROLE as a user: %d %s", r.Code, r.Body)
	}
	for _, fn := range []string{"escape_auth", "escape_service"} {
		for _, tok := range []string{"", ngoziAAL2.AccessToken} {
			if r := api.call("POST", "/data/v1/rpc/"+fn, `{}`, tok); r.Code == 200 || !strings.Contains(r.Body, "permission denied") {
				t.Fatalf("%s: %d %s", fn, r.Code, r.Body)
			}
		}
	}
	// The edge login holds no request role.
	var memberships int
	admin, err := e.Service.AdminConn(ctx, p.InstanceID, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.member WHERE r.rolname = $1`,
		store.EdgeRole(p.DbName)).Scan(&memberships); err != nil || memberships != 0 {
		t.Fatalf("the edge login's memberships: %d %v", memberships, err)
	}
}
