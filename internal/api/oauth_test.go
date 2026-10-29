package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/orgs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
	"github.com/israel-duff/xenibase/internal/tokens"
)

// newOAuthServer is newAuthServer with API tokens enabled and a step-up
// window long enough for a whole test.
func newOAuthServer(t *testing.T) (*httptest.Server, *fakeClock, DB) {
	t.Helper()
	pool := storetest.New(t)
	key, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(key)
	clk := &fakeClock{t: time.Now()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := auth.NewService(pool, kr, auth.Config{Now: clk.Now, ReauthWindow: time.Hour}, "let-me-in", log)
	if err := svc.EnsureTerms(context.Background()); err != nil {
		t.Fatal(err)
	}
	orgSvc := orgs.New(pool, svc, nil, nil, "", log)
	svc.SetHooks(orgSvc.Hooks())
	ts := httptest.NewServer(NewHandler(Options{
		Logger: log, DB: pool, Auth: svc, Orgs: orgSvc, Now: clk.Now,
		Tokens: tokens.New(pool, kr, nil, tokens.Config{Now: clk.Now}, log),
		UI:     fstest.MapFS{"index.html": {Data: []byte("app")}}, UIIndex: "index.html",
	}))
	t.Cleanup(ts.Close)
	return ts, clk, pool
}

// app is a third-party app: no cookies, form-encoded or JSON bodies.
type app struct {
	t    *testing.T
	base string
}

// post sends form fields (or JSON when asJSON) and decodes the answer.
func (a *app) post(path string, fields map[string]string, asJSON bool, basic ...string) (int, map[string]any) {
	a.t.Helper()
	var req *http.Request
	if asJSON {
		b, _ := json.Marshal(fields)
		req, _ = http.NewRequest("POST", a.base+path, strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
	} else {
		v := url.Values{}
		for k, val := range fields {
			v.Set(k, val)
		}
		req, _ = http.NewRequest("POST", a.base+path, strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if len(basic) == 2 {
		req.SetBasicAuth(basic[0], basic[1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return res.StatusCode, out
}

// bearer calls the API with an issued token.
func (a *app) bearer(token, method, path string) (int, string) {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.base+path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// seedProject puts a shared project in org, bypassing provisioning.
func seedProject(t *testing.T, db DB, org uuid.UUID, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	node, inst := uuid.New(), uuid.New()
	for _, stmt := range []string{
		`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ('` + node.String() + `', 'n-` + name + `', '10.0.0.1', 'both', 'fp', '{}')`,
		`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ('` + inst.String() + `', '` + node.String() + `', 'shared', 18, 5432, 'running')`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	p, err := store.New(db).InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: org, Name: name, Slug: name, DbName: "p_" + name, OwnerRole: "p_" + name + "_owner",
		ScramVerifier: "x", Tier: "shared", InstanceID: inst, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

const (
	redirectURI = "https://app.taskiem.test/connections/xenibase/callback?src=xb"
	verifier    = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
)

func authorizeQuery(clientID, challenge, scope, state string) string {
	v := url.Values{"client_id": {clientID}, "redirect_uri": {redirectURI}, "scope": {scope}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	return "/api/v1/oauth/authorize?" + v.Encode()
}

func consent(t *testing.T, b *browser, clientID, challenge, state string, approve bool, extra map[string]any) string {
	t.Helper()
	// The page's first call seals the request; the answer brings it back.
	var info gen.OAuthAuthorizeInfo
	if code, raw := b.do("GET", authorizeQuery(clientID, challenge, "", state), nil, &info); code != 200 {
		t.Fatalf("consent page: %d %s", code, raw)
	}
	body := map[string]any{"client_id": clientID, "redirect_uri": redirectURI, "state": state, "code_challenge": challenge,
		"code_challenge_method": "S256", "approve": approve, "consent": info.Consent}
	for k, v := range extra {
		body[k] = v
	}
	var out gen.OAuthRedirect
	if code, raw := b.do("POST", "/api/v1/oauth/authorize", body, &out); code != 200 {
		t.Fatalf("consent: %d %s", code, raw)
	}
	return out.RedirectTo
}

func codeFrom(t *testing.T, redirect, state string) string {
	t.Helper()
	u, err := url.Parse(redirect)
	if err != nil || !strings.HasPrefix(redirect, "https://app.taskiem.test/connections/xenibase/callback?") {
		t.Fatalf("redirect %q: %v", redirect, err)
	}
	q := u.Query()
	if q.Get("src") != "xb" || q.Get("state") != state || q.Get("code") == "" || q.Get("error") != "" {
		t.Fatalf("redirect query %v", q)
	}
	return q.Get("code")
}

// TestOAuthFlowOverHTTP runs a third-party app through the whole flow:
// registration, consent, the exchange with PKCE, the token's limits, the
// single use of a code, revocation, and what shows on the Account page.
func TestOAuthFlowOverHTTP(t *testing.T) {
	ts, clk, db := newOAuthServer(t)
	admin := newBrowser(t, ts.URL)
	admin.setup(clk)
	taskiem := &app{t: t, base: ts.URL}
	challenge := tokens.PKCEChallenge(verifier)

	// Registration: platform admin only, https redirects, a secret shown once.
	var created gen.OAuthClientCreated
	if code, body := admin.do("POST", "/api/v1/admin/oauth/clients", map[string]any{
		"name": "Taskiem", "redirect_uris": []string{redirectURI, "http://localhost:3000/cb"}, "scopes_allowed": []string{"read", "write"},
	}, &created); code != 201 || created.ClientSecret == nil || !strings.HasPrefix(*created.ClientSecret, "xbc_") || !strings.HasPrefix(created.Client.ClientId, "xbapp_") || !created.Client.Confidential {
		t.Fatalf("register: %d %s", code, body)
	}
	clientID, secret := created.Client.ClientId, *created.ClientSecret
	if code, body := admin.do("POST", "/api/v1/admin/oauth/clients", map[string]any{"name": "Bad", "redirect_uris": []string{"http://example.com/cb"}}, nil); code != 400 || !strings.Contains(body, "https") {
		t.Fatalf("http redirect accepted: %d %s", code, body)
	}
	var list gen.OAuthClientList
	if code, _ := admin.do("GET", "/api/v1/admin/oauth/clients", nil, &list); code != 200 || len(list.Items) != 1 || list.Items[0].Name != "Taskiem" {
		t.Fatalf("list clients: %d %+v", code, list)
	}

	// The organisation and a project to restrict to.
	var ol gen.OrgList
	if code, _ := admin.do("GET", "/api/v1/orgs", nil, &ol); code != 200 || len(ol.Items) == 0 {
		t.Fatalf("orgs: %d", code)
	}
	org := ol.Items[0].Id
	project := seedProject(t, db, org, "crm")
	other := seedProject(t, db, org, "hr")

	// Signed out, the consent page's call is refused.
	anon := newBrowser(t, ts.URL)
	if code, _ := anon.do("GET", authorizeQuery(clientID, challenge, "read write", "s1"), nil, nil); code != 401 {
		t.Fatalf("anonymous authorize: %d", code)
	}
	// What the page shows.
	var info gen.OAuthAuthorizeInfo
	if code, body := admin.do("GET", authorizeQuery(clientID, challenge, "read write", "s1"), nil, &info); code != 200 {
		t.Fatalf("authorize info: %d %s", code, body)
	}
	if info.ClientName != "Taskiem" || len(info.Scopes) != 2 || len(info.Organisations) == 0 || info.Organisations[0].Id != org || len(info.Organisations[0].Projects) != 2 || *info.State != "s1" {
		t.Fatalf("authorize info: %+v", info)
	}
	for _, bad := range []string{
		"client_id=xbapp_nope&redirect_uri=" + url.QueryEscape(redirectURI) + "&code_challenge=" + challenge + "&code_challenge_method=S256",
		"client_id=" + clientID + "&redirect_uri=https%3A%2F%2Fevil.test%2Fcb&code_challenge=" + challenge + "&code_challenge_method=S256",
		"client_id=" + clientID + "&redirect_uri=" + url.QueryEscape(redirectURI) + "&scope=admin&code_challenge=" + challenge + "&code_challenge_method=S256",
		"client_id=" + clientID + "&redirect_uri=" + url.QueryEscape(redirectURI) + "&code_challenge=" + verifier + "&code_challenge_method=plain",
	} {
		if code, body := admin.do("GET", "/api/v1/oauth/authorize?"+bad, nil, nil); code != 400 {
			t.Fatalf("authorize %s: %d %s", bad, code, body)
		}
	}

	// Scopes can't be widened beyond the request (the app may ask for
	// read and write; the user chose read).
	approve := func(consent string, scopes ...string) (int, string) {
		return admin.do("POST", "/api/v1/oauth/authorize", map[string]any{
			"client_id": clientID, "redirect_uri": redirectURI, "code_challenge": challenge, "code_challenge_method": "S256",
			"approve": true, "org_id": org, "scopes": scopes, "consent": consent,
		}, nil)
	}
	if code, body := approve(info.Consent, "read", "write", "admin"); code != 400 || !strings.Contains(body, "admin") {
		t.Fatalf("widened scopes: %d %s", code, body)
	}
	// The request is the server's record, not the page's: an app that asked
	// for read gets no more, whatever the answer says.
	var readOnly gen.OAuthAuthorizeInfo
	if code, body := admin.do("GET", authorizeQuery(clientID, challenge, "read", "s1"), nil, &readOnly); code != 200 || len(readOnly.Scopes) != 1 {
		t.Fatalf("a read-only request: %d %s", code, body)
	}
	if code, body := approve(readOnly.Consent, "read", "write"); code != 400 || !strings.Contains(body, "write") {
		t.Fatalf("widened past the request: %d %s", code, body)
	}
	for _, bad := range []string{"", "not-a-consent", readOnly.Consent[:len(readOnly.Consent)-4] + "AAAA"} {
		if code, body := approve(bad, "read"); code != 400 || !strings.Contains(body, "consent") {
			t.Fatalf("approving with consent %q: %d %s", bad, code, body)
		}
	}
	// Denied: the app learns so at its redirect.
	if to := consent(t, admin, clientID, challenge, "s2", false, nil); !strings.Contains(to, "error=access_denied") || !strings.Contains(to, "state=s2") || strings.Contains(to, "code=") {
		t.Fatalf("deny redirect: %s", to)
	}
	// Approved: read only, one project, 30 days.
	code := codeFrom(t, consent(t, admin, clientID, challenge, "s3", true, map[string]any{
		"org_id": org, "scopes": []string{"read"}, "project_ids": []uuid.UUID{project}, "expires_in_days": 30,
	}), "s3")

	exchange := map[string]string{"grant_type": "authorization_code", "code": code, "redirect_uri": redirectURI,
		"client_id": clientID, "client_secret": secret, "code_verifier": verifier}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range exchange {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	// The wrong verifier, secret, redirect or grant type: refused in
	// OAuth's shape, and the code is still unused.
	if st, out := taskiem.post("/api/v1/oauth/token", with("code_verifier", strings.Repeat("x", 43)), false); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %v", st, out)
	}
	if st, out := taskiem.post("/api/v1/oauth/token", with("client_secret", "xbc_wrong"), false); st != 401 || out["error"] != "invalid_client" {
		t.Fatalf("wrong secret: %d %v", st, out)
	}
	if st, out := taskiem.post("/api/v1/oauth/token", with("redirect_uri", "https://app.taskiem.test/other"), false); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("wrong redirect: %d %v", st, out)
	}
	if st, out := taskiem.post("/api/v1/oauth/token", with("grant_type", "client_credentials"), false); st != 400 || out["error"] != "unsupported_grant_type" {
		t.Fatalf("grant type: %d %v", st, out)
	}
	st, tok := taskiem.post("/api/v1/oauth/token", exchange, false)
	access, _ := tok["access_token"].(string)
	if st != 200 || !strings.HasPrefix(access, "xb_") || tok["token_type"] != "Bearer" || tok["scope"] != "read" || tok["org_id"] != org.String() {
		t.Fatalf("exchange: %d %v", st, tok)
	}
	if ids, _ := tok["project_ids"].([]any); len(ids) != 1 || ids[0] != project.String() {
		t.Fatalf("project restriction: %v", tok["project_ids"])
	}
	if exp, _ := tok["expires_in"].(float64); exp < 29*24*3600 || exp > 30*24*3600 {
		t.Fatalf("expires_in: %v", tok["expires_in"])
	}

	// The token works, and only as granted.
	if st, body := taskiem.bearer(access, "GET", "/api/v1/me"); st != 200 || !strings.Contains(body, ownerEmail) {
		t.Fatalf("me with token: %d %s", st, body)
	}
	if st, body := taskiem.bearer(access, "POST", "/api/v1/tokens"); st != 403 || !strings.Contains(body, "insufficient_scope") {
		t.Fatalf("write with a read token: %d %s", st, body)
	}
	if st, body := taskiem.bearer(access, "GET", "/api/v1/projects/"+other.String()); st != 404 {
		t.Fatalf("other project with a restricted token: %d %s", st, body)
	}
	// On the Account page, with the app's name.
	var mine gen.APITokenList
	if st, _ := admin.do("GET", "/api/v1/tokens", nil, &mine); st != 200 || len(mine.Items) != 1 {
		t.Fatalf("my tokens: %d %+v", st, mine)
	}
	if tk := mine.Items[0]; tk.CreatedVia != gen.APITokenCreatedViaOauth || tk.OauthClientName == nil || *tk.OauthClientName != "Taskiem" || tk.Name != "Taskiem" {
		t.Fatalf("listed token: %+v", tk)
	}
	tokenID := mine.Items[0].Id

	// A code works once: the second exchange fails and revokes the token
	// (RFC 6749 s4.1.2).
	if st, out := taskiem.post("/api/v1/oauth/token", exchange, true); st != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("second exchange: %d %v", st, out)
	}
	if st, body := taskiem.bearer(access, "GET", "/api/v1/me"); st != 401 || !strings.Contains(body, "invalid_token") {
		t.Fatalf("token after code reuse: %d %s", st, body)
	}
	if st, _ := admin.do("GET", "/api/v1/tokens", nil, &mine); st != 200 || mine.Items[0].Id != tokenID || mine.Items[0].Status != "revoked" {
		t.Fatalf("token should show revoked: %+v", mine.Items)
	}

	// An expired code.
	code = codeFrom(t, consent(t, admin, clientID, challenge, "s4", true, map[string]any{"org_id": org}), "s4")
	clk.mu.Lock()
	clk.t = clk.t.Add(11 * time.Minute)
	clk.mu.Unlock()
	if st, out := taskiem.post("/api/v1/oauth/token", with("code", code), false); st != 400 || out["error"] != "invalid_grant" || !strings.Contains(out["error_description"].(string), "expired") {
		t.Fatalf("expired code: %d %v", st, out)
	}

	// Revocation by the app (RFC 7009), with HTTP Basic client auth; the
	// default scopes and expiry apply when the user chose none.
	code = codeFrom(t, consent(t, admin, clientID, challenge, "s5", true, map[string]any{"org_id": org}), "s5")
	st, tok = taskiem.post("/api/v1/oauth/token", map[string]string{"grant_type": "authorization_code", "code": code,
		"redirect_uri": redirectURI, "code_verifier": verifier}, true, clientID, secret)
	access, _ = tok["access_token"].(string)
	if st != 200 || tok["scope"] != "read write" || tok["project_ids"] != nil {
		t.Fatalf("exchange with basic auth: %d %v", st, tok)
	}
	if exp, _ := tok["expires_in"].(float64); exp < 89*24*3600 || exp > 90*24*3600 {
		t.Fatalf("default expiry: %v", tok["expires_in"])
	}
	if st, body := taskiem.bearer(access, "GET", "/api/v1/me"); st != 200 {
		t.Fatalf("second token: %d %s", st, body)
	}
	// With write, it still can't make tokens of its own, which disconnecting
	// the app would leave behind.
	mint, _ := http.NewRequest("POST", ts.URL+"/api/v1/tokens", strings.NewReader(`{"name":"mine","org_id":"`+org.String()+`","scopes":["read"],"expires_in_days":7}`))
	mint.Header.Set("Content-Type", "application/json")
	mint.Header.Set("Authorization", "Bearer "+access)
	if res, err := http.DefaultClient.Do(mint); err != nil {
		t.Fatal(err)
	} else {
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden || !strings.Contains(string(raw), "token_not_allowed") {
			t.Fatalf("an app's token creating a token: %d %s", res.StatusCode, raw)
		}
	}
	if st, out := taskiem.post("/api/v1/oauth/revoke", map[string]string{"token": access, "client_id": clientID, "client_secret": "xbc_wrong"}, false); st != 401 || out["error"] != "invalid_client" {
		t.Fatalf("revoke with the wrong secret: %d %v", st, out)
	}
	if st, body := taskiem.bearer(access, "GET", "/api/v1/me"); st != 200 {
		t.Fatalf("token after a refused revoke: %d %s", st, body)
	}
	for range 2 { // idempotent
		if st, out := taskiem.post("/api/v1/oauth/revoke", map[string]string{"token": access}, false, clientID, secret); st != 200 {
			t.Fatalf("revoke: %d %v", st, out)
		}
	}
	if st, body := taskiem.bearer(access, "GET", "/api/v1/me"); st != 401 {
		t.Fatalf("revoked token: %d %s", st, body)
	}

	// Revoked from Xenibase instead: the same 401.
	code = codeFrom(t, consent(t, admin, clientID, challenge, "s6", true, map[string]any{"org_id": org}), "s6")
	st, tok = taskiem.post("/api/v1/oauth/token", with("code", code), false)
	access, _ = tok["access_token"].(string)
	if st != 200 {
		t.Fatalf("third exchange: %d %v", st, tok)
	}
	if st, body := admin.do("DELETE", "/api/v1/tokens/"+tok["token_id"].(string), nil, nil); st != 204 {
		t.Fatalf("revoke from account: %d %s", st, body)
	}
	if st, body := taskiem.bearer(access, "GET", "/api/v1/me"); st != 401 || !strings.Contains(body, "invalid_token") {
		t.Fatalf("token revoked from the account: %d %s", st, body)
	}

	// A public client: PKCE alone, and a secret is refused.
	var public gen.OAuthClientCreated
	if st, body := admin.do("POST", "/api/v1/admin/oauth/clients", map[string]any{"name": "Taskiem CLI", "redirect_uris": []string{redirectURI}, "confidential": false}, &public); st != 201 || public.ClientSecret != nil || public.Client.Confidential {
		t.Fatalf("public client: %d %s", st, body)
	}
	code = codeFrom(t, consent(t, admin, public.Client.ClientId, challenge, "s7", true, map[string]any{"org_id": org}), "s7")
	pub := map[string]string{"grant_type": "authorization_code", "code": code, "redirect_uri": redirectURI, "client_id": public.Client.ClientId, "code_verifier": verifier}
	if st, out := taskiem.post("/api/v1/oauth/token", pub, false, public.Client.ClientId, "xbc_x"); st != 401 || out["error"] != "invalid_client" {
		t.Fatalf("public client with a secret: %d %v", st, out)
	}
	if st, out := taskiem.post("/api/v1/oauth/token", pub, false); st != 200 {
		t.Fatalf("public client exchange: %d %v", st, out)
	}

	// Disabling the app ends everything it was issued.
	code = codeFrom(t, consent(t, admin, clientID, challenge, "s8", true, map[string]any{"org_id": org}), "s8")
	st, tok = taskiem.post("/api/v1/oauth/token", with("code", code), false)
	access, _ = tok["access_token"].(string)
	if st != 200 {
		t.Fatalf("fourth exchange: %d %v", st, tok)
	}
	if st, body := admin.do("DELETE", "/api/v1/admin/oauth/clients/"+clientID, nil, nil); st != 204 {
		t.Fatalf("disable client: %d %s", st, body)
	}
	if st, body := taskiem.bearer(access, "GET", "/api/v1/me"); st != 401 {
		t.Fatalf("token of a disabled app: %d %s", st, body)
	}
	if st, body := admin.do("GET", authorizeQuery(clientID, challenge, "read", "s9"), nil, nil); st != 400 || !strings.Contains(body, "disabled") {
		t.Fatalf("authorize for a disabled app: %d %s", st, body)
	}
	if st, out := taskiem.post("/api/v1/oauth/token", exchange, false); st != 401 || out["error"] != "invalid_client" {
		t.Fatalf("exchange for a disabled app: %d %v", st, out)
	}
	if st, _ := admin.do("DELETE", "/api/v1/admin/oauth/clients/xbapp_nope", nil, nil); st != 404 {
		t.Fatalf("disable unknown client: %d", st)
	}
	// The audit log has the consent, the issue and the revocations.
	var actions []string
	rows, err := db.Query(context.Background(), `SELECT action FROM audit_log WHERE action LIKE 'oauth.%' OR action LIKE 'admin.oauth%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	joined := strings.Join(actions, " ")
	for _, want := range []string{"admin.oauth_client.create", "oauth.authorize", "oauth.token_issue", "oauth.token_revoke", "admin.oauth_client.disable"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit log lacks %s: %v", want, actions)
		}
	}
}

// TestIntegratorRolesRefusesSharedProjects covers the operator action's
// checks that need no dedicated instance: validation, an unknown project,
// and the shared tier.
func TestIntegratorRolesRefusesSharedProjects(t *testing.T) {
	ts, clk, db := newOAuthServer(t)
	admin := newBrowser(t, ts.URL)
	admin.setup(clk)
	var ol gen.OrgList
	if code, _ := admin.do("GET", "/api/v1/orgs", nil, &ol); code != 200 || len(ol.Items) == 0 {
		t.Fatalf("orgs: %d", code)
	}
	project := seedProject(t, db, ol.Items[0].Id, "shared")
	path := "/api/v1/admin/projects/" + project.String() + "/roles"
	if code, body := admin.do("POST", path, map[string]any{"roles": []string{"Taskiem"}}, nil); code != 400 || !strings.Contains(body, "bad_request") {
		t.Fatalf("bad role name: %d %s", code, body)
	}
	if code, body := admin.do("POST", path, map[string]any{"roles": []string{"pg_taskiem"}}, nil); code != 400 || !strings.Contains(body, "reserved") {
		t.Fatalf("reserved role name: %d %s", code, body)
	}
	if code, body := admin.do("POST", "/api/v1/admin/projects/"+uuid.NewString()+"/roles", map[string]any{"roles": []string{"taskiem_app"}}, nil); code != 404 {
		t.Fatalf("unknown project: %d %s", code, body)
	}
	if code, body := admin.do("POST", path, map[string]any{"roles": []string{"taskiem_app", "taskiem_dispatch"}}, nil); code != 409 || !strings.Contains(body, "dedicated projects only") {
		t.Fatalf("shared project: %d %s", code, body)
	}
	// Each attempt is audited, as a failure.
	var n, conflicts int
	if err := db.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE detail->>'status' = '409') FROM audit_log
		WHERE action = 'admin.project.integrator_roles' AND outcome = 'failure'`).Scan(&n, &conflicts); err != nil || n != 4 || conflicts != 1 {
		t.Fatalf("audit rows: %d (%d conflicts) %v", n, conflicts, err)
	}
}
