package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/test/testenv"
)

func sqlBody(q string) map[string]any {
	return map[string]any{"query": q, "query_id": uuid.New()}
}

func errCode(b []byte) string {
	var e gen.Error
	_ = json.Unmarshal(b, &e)
	return e.Code
}

// TestRestrictedWriteTokenInCI is M10's done-when (V2 §7.2): a CI job with
// a project-restricted write token runs SQL on its project, gets 404 for a
// project in another organisation, and is refused when it tries to delete
// its own project.
func TestRestrictedWriteTokenInCI(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	blog := e.CreateProject("Blog")
	sibling := e.CreateProject("Sibling") // same org, outside the restriction
	team := e.CreateOrg("Other team")
	shop := e.CreateProjectIn("Shop", team) // another of the owner's orgs
	bob := e.InviteUser("bob@example.com")
	bobs := bob.CreateProject("Bobs app", bob.OrgID) // someone else's org

	token := e.CreateToken(map[string]any{
		"name": "GitHub Actions — blog", "org_id": e.OrgID, "scopes": []string{"write"},
		"project_ids": []uuid.UUID{blog.Project.Id}, "expires_in_days": 30,
	})
	if !strings.HasPrefix(token, "xb_") || len(token) != 46 {
		t.Fatalf("token format: %q", token)
	}
	blogPath := "/api/v1/projects/" + blog.Project.Id.String()

	// SQL on its project, reads and writes.
	var res gen.SqlResult
	code, raw := e.BearerDo(token, "POST", blogPath+"/sql", sqlBody("CREATE TABLE ci_runs (id int); INSERT INTO ci_runs VALUES (1), (2); SELECT count(*) AS n FROM ci_runs"), &res)
	if code != http.StatusOK || res.Error != nil || len(res.Results) == 0 {
		t.Fatalf("sql with the token: %d %s", code, raw)
	}
	last := res.Results[len(res.Results)-1]
	if len(last.Rows) != 1 || last.Rows[0][0] == nil || *last.Rows[0][0] != "2" {
		t.Fatalf("sql result: %s", raw)
	}

	// 404 for a project in another organisation, its own user's or anyone's,
	// and for the project in its own org it isn't restricted to.
	for name, id := range map[string]uuid.UUID{"other org": shop.Project.Id, "someone else's org": bobs.Project.Id, "unrestricted sibling": sibling.Project.Id} {
		if code, raw := e.BearerDo(token, "GET", "/api/v1/projects/"+id.String(), nil, nil); code != http.StatusNotFound {
			t.Errorf("%s: %d %s", name, code, raw)
		}
		if code, _ := e.BearerDo(token, "POST", "/api/v1/projects/"+id.String()+"/sql", sqlBody("SELECT 1"), nil); code != http.StatusNotFound {
			t.Errorf("%s sql: %d", name, code)
		}
	}
	var list gen.ProjectList
	if code, _ := e.BearerDo(token, "GET", "/api/v1/projects", nil, &list); code != http.StatusOK || len(list.Items) != 1 || list.Items[0].Id != blog.Project.Id {
		t.Fatalf("project list with the token: %d %+v", code, list.Items)
	}
	if code, _ := e.BearerDo(token, "GET", "/api/v1/projects?org="+team.String(), nil, nil); code != http.StatusNotFound {
		t.Fatalf("another org's project list: %d", code)
	}

	// Refused when it tries to delete its own project, confirmed or not.
	code, raw = e.BearerDo(token, "DELETE", blogPath+"?confirm=Blog", nil, nil)
	if code != http.StatusForbidden || errCode(raw) != "insufficient_scope" {
		t.Fatalf("delete with a write token: %d %s", code, raw)
	}
	var p gen.Project
	if code := e.Do("GET", blogPath, nil, &p); code != http.StatusOK || p.Status != gen.ProjectStatusActive {
		t.Fatalf("the project after the refused delete: %d %s", code, p.Status)
	}

	// The request is audited as the token.
	var actor, tokenID string
	if err := e.DB.QueryRow(context.Background(),
		`SELECT actor_kind, token_id::text FROM audit_log WHERE action = 'project.console' AND project_id = $1 ORDER BY created_at DESC LIMIT 1`,
		blog.Project.Id).Scan(&actor, &tokenID); err != nil || actor != "token" || tokenID == "" {
		t.Fatalf("audit: %q %q %v", actor, tokenID, err)
	}
}

// TestAPITokens covers V2 §7.2's rules.
func TestAPITokens(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	app := e.CreateProject("App")
	appPath := "/api/v1/projects/" + app.Project.Id.String()

	// Scopes: admin requires write; unknown scopes and over-long expiries fail.
	e.Reauth()
	for _, bad := range []map[string]any{
		{"name": "x", "org_id": e.OrgID, "scopes": []string{"admin"}},
		{"name": "x", "org_id": e.OrgID, "scopes": []string{"read"}, "expires_in_days": 400},
		{"name": "", "org_id": e.OrgID, "scopes": []string{"read"}},
		{"name": "x", "org_id": uuid.New(), "scopes": []string{"read"}},
	} {
		if code := e.Do("POST", "/api/v1/tokens", bad, nil); code != http.StatusBadRequest && code != http.StatusNotFound {
			t.Errorf("bad token %v: %d", bad, code)
		}
	}

	read := e.CreateToken(map[string]any{"name": "laptop", "org_id": e.OrgID, "scopes": []string{"read"}})
	if e.SMTP.Count(testenv.OwnerEmail, "New Xenibase API token") == 0 {
		t.Error("no creation email")
	}

	// whoami: the token and its organisation only.
	var me gen.User
	if code, _ := e.BearerDo(read, "GET", "/api/v1/me", nil, &me); code != http.StatusOK || me.Token == nil || me.Token.OrgId != e.OrgID {
		t.Fatalf("whoami: %d %+v", code, me)
	}
	// The expiry is the token's own (the default is 90 days); it used to come
	// back as the zero time.
	if d := time.Until(me.Token.ExpiresAt); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Errorf("whoami expires_at = %s, want about 90 days from now", me.Token.ExpiresAt)
	}
	team := e.CreateOrg("Team")
	var orgs gen.OrgList
	if code, _ := e.BearerDo(read, "GET", "/api/v1/orgs", nil, &orgs); code != http.StatusOK || len(orgs.Items) != 1 || orgs.Items[0].Id != e.OrgID {
		t.Fatalf("orgs with a token: %d %+v", code, orgs.Items)
	}
	if code, _ := e.BearerDo(read, "GET", "/api/v1/orgs/"+team.String(), nil, nil); code != http.StatusNotFound {
		t.Fatalf("another org with the token: %d", code)
	}

	// Read scope: reads, read-only SQL, no writes.
	if code, _ := e.BearerDo(read, "GET", appPath, nil, nil); code != http.StatusOK {
		t.Fatalf("read: %d", code)
	}
	if code, raw := e.BearerDo(read, "POST", appPath+"/sql", sqlBody("CREATE TABLE nope (id int)"), nil); code != http.StatusForbidden || errCode(raw) != "insufficient_scope" {
		t.Fatalf("write sql with a read token: %d %s", code, raw)
	}
	ro := sqlBody("SELECT 1")
	ro["read_only"] = true
	if code, raw := e.BearerDo(read, "POST", appPath+"/sql", ro, nil); code != http.StatusOK {
		t.Fatalf("read-only sql with a read token: %d %s", code, raw)
	}
	if code, _ := e.BearerDo(read, "POST", appPath+"/backups", nil, nil); code != http.StatusForbidden {
		t.Fatalf("backup with a read token: %d", code)
	}

	// Tokens never reach the platform or session-only routes, and no CSRF
	// token is needed (or used).
	for _, path := range []string{"/api/v1/nodes", "/api/v1/admin/users", "/api/v1/me/sessions", "/api/v1/me/recovery-codes"} {
		if code, raw := e.BearerDo(read, "GET", path, nil, nil); code != http.StatusForbidden {
			t.Errorf("%s with a token: %d %s", path, code, raw)
		}
	}
	if code, raw := e.BearerDo("xb_notarealtoken0000000000000000000000000000", "GET", "/api/v1/me", nil, nil); code != http.StatusUnauthorized || errCode(raw) != "invalid_token" {
		t.Fatalf("a made-up token: %d %s", code, raw)
	}

	// Admin scope: destructive actions with a typed confirm, no step-up.
	admin := e.CreateToken(map[string]any{"name": "ops", "org_id": e.OrgID, "scopes": []string{"write", "admin"}})
	if code, _ := e.BearerDo(admin, "DELETE", appPath+"?confirm=wrong", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("delete with the wrong confirm: %d", code)
	}
	// A token can create tokens, never more than itself.
	if code, _ := e.BearerDo(read, "POST", "/api/v1/tokens", map[string]any{"name": "x", "org_id": e.OrgID, "scopes": []string{"read"}}, nil); code != http.StatusForbidden {
		t.Fatalf("a read token creating a token: %d", code)
	}
	var made gen.CreatedToken
	if code, raw := e.BearerDo(admin, "POST", "/api/v1/tokens", map[string]any{"name": "child", "org_id": e.OrgID, "scopes": []string{"read"}, "expires_in_days": 30}, &made); code != http.StatusCreated || made.Token.CreatedVia != "api" {
		t.Fatalf("a token creating a narrower token: %d %s", code, raw)
	}
	if code, _ := e.BearerDo(admin, "POST", "/api/v1/tokens", map[string]any{"name": "x", "org_id": e.OrgID, "scopes": []string{"read"}, "expires_in_days": 365}, nil); code != http.StatusForbidden {
		t.Fatalf("a token creating a token that outlives it: %d", code)
	}

	// Last used is recorded.
	var lastUsed *time.Time
	if err := e.DB.QueryRow(ctx, `SELECT last_used_at FROM api_tokens WHERE id = $1`, made.Token.Id).Scan(&lastUsed); err != nil || lastUsed != nil {
		t.Fatalf("unused token's last use: %v %v", lastUsed, err)
	}
	e.BearerDo(made.Secret, "GET", "/api/v1/me", nil, nil)
	if err := e.DB.QueryRow(ctx, `SELECT last_used_at FROM api_tokens WHERE id = $1`, made.Token.Id).Scan(&lastUsed); err != nil || lastUsed == nil {
		t.Fatalf("last use not recorded: %v", err)
	}

	// Revoking ends it at once.
	if code := e.Do("DELETE", "/api/v1/tokens/"+made.Token.Id.String(), nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := e.BearerDo(made.Secret, "GET", "/api/v1/me", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", code)
	}

	// Removal from the organisation revokes the member's tokens; owners and
	// admins see and revoke any token scoped to their org.
	var inv gen.InvitationCreated
	if code := e.Do("POST", "/api/v1/orgs/"+team.String()+"/members", map[string]any{"email": "carol@example.com", "role": "member"}, &inv); code != http.StatusCreated {
		t.Fatalf("invite carol: %d", code)
	}
	carol := e.AcceptInvitation(e.MailToken("carol@example.com", "invitation"), "carol@example.com")
	var ct gen.CreatedToken
	carol.Reauth()
	if code := carol.Do("POST", "/api/v1/tokens", map[string]any{"name": "carol ci", "org_id": team, "scopes": []string{"read"}}, &ct); code != http.StatusCreated {
		t.Fatalf("carol's token: %d", code)
	}
	var orgTokens gen.APITokenList
	if code := e.Do("GET", "/api/v1/orgs/"+team.String()+"/tokens", nil, &orgTokens); code != http.StatusOK || len(orgTokens.Items) != 1 || *orgTokens.Items[0].UserEmail != "carol@example.com" {
		t.Fatalf("org tokens: %d %+v", code, orgTokens.Items)
	}
	if code := carol.Do("GET", "/api/v1/orgs/"+team.String()+"/tokens", nil, nil); code != http.StatusForbidden {
		t.Fatalf("a member listing org tokens: %d", code)
	}
	if code, _ := e.BearerDo(ct.Secret, "GET", "/api/v1/orgs/"+team.String(), nil, nil); code != http.StatusOK {
		t.Fatalf("carol's token before removal: %d", code)
	}
	if code := e.Do("DELETE", "/api/v1/orgs/"+team.String()+"/members/"+carol.UserID.String(), nil, nil); code != http.StatusNoContent {
		t.Fatalf("remove carol: %d", code)
	}
	if code, _ := e.BearerDo(ct.Secret, "GET", "/api/v1/orgs/"+team.String(), nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("carol's token after removal: %d", code)
	}

	// Suspending the organisation disables its tokens.
	teamToken := e.CreateToken(map[string]any{"name": "team", "org_id": team, "scopes": []string{"read"}})
	if err := e.Tenancy.Suspend(ctx, team, "unpaid"); err != nil {
		t.Fatal(err)
	}
	if code, raw := e.BearerDo(teamToken, "GET", "/api/v1/orgs/"+team.String(), nil, nil); code != http.StatusForbidden || errCode(raw) != "org_suspended" {
		t.Fatalf("suspended org's token: %d %s", code, raw)
	}

	// Expiry: a reminder a week before, then the token stops working.
	e.Advance(84 * 24 * time.Hour)
	if err := e.Tokens.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if e.SMTP.Count(testenv.OwnerEmail, "expires soon") == 0 {
		t.Error("no expiry reminder")
	}
	e.Advance(7 * 24 * time.Hour)
	if code, _ := e.BearerDo(read, "GET", "/api/v1/me", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", code)
	}
}

// TestTokenRateLimit: bearer requests are limited per token.
func TestTokenRateLimit(t *testing.T) {
	e := testenv.Start(t, testenv.Options{TokenRate: 5})
	tok := e.CreateToken(map[string]any{"name": "busy", "org_id": e.OrgID, "scopes": []string{"read"}})
	for i := range 5 {
		if code, _ := e.BearerDo(tok, "GET", "/api/v1/me", nil, nil); code != http.StatusOK {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if code, raw := e.BearerDo(tok, "GET", "/api/v1/me", nil, nil); code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d %s", code, raw)
	}
	// Another token has its own budget.
	other := e.CreateToken(map[string]any{"name": "quiet", "org_id": e.OrgID, "scopes": []string{"read"}})
	if code, _ := e.BearerDo(other, "GET", "/api/v1/me", nil, nil); code != http.StatusOK {
		t.Fatalf("another token: %d", code)
	}
}

// TestDeviceLogin is the CLI's login (V2 §7.1): a code, approval in the
// browser choosing the org and scopes, and the token collected once.
func TestDeviceLogin(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	team := e.CreateOrg("CLI team")

	var start gen.DeviceAuthorization
	if code, raw := e.BearerDo("", "POST", "/api/v1/auth/device", map[string]any{"client_name": "xenibase CLI on laptop", "scopes": []string{"read", "write"}}, &start); code != http.StatusOK {
		t.Fatalf("start: %d %s", code, raw)
	}
	if !strings.HasPrefix(start.VerificationUriComplete, "https://xenibase.test/device?code=") || len(start.UserCode) != 9 {
		t.Fatalf("start: %+v", start)
	}
	poll := func() (int, string, gen.CreatedToken) {
		var out gen.CreatedToken
		code, raw := e.BearerDo("", "POST", "/api/v1/auth/device/token", map[string]any{"device_code": start.DeviceCode}, &out)
		return code, errCode(raw), out
	}
	if _, c, _ := poll(); c != "authorization_pending" {
		t.Fatalf("before approval: %s", c)
	}
	if _, c, _ := poll(); c != "slow_down" {
		t.Fatalf("polling too fast: %s", c)
	}

	var req gen.DeviceRequest
	if code := e.Do("GET", "/api/v1/auth/device/requests/"+strings.ToLower(start.UserCode), nil, &req); code != http.StatusOK || req.ClientName != "xenibase CLI on laptop" {
		t.Fatalf("lookup: %d %+v", code, req)
	}
	// Approving needs a recent step-up; more than the CLI asked for is refused.
	e.Reauth()
	if code := e.Do("POST", "/api/v1/auth/device/approve", map[string]any{"user_code": start.UserCode, "org_id": team, "scopes": []string{"write", "admin"}}, nil); code != http.StatusBadRequest {
		t.Fatalf("approve with admin: %d", code)
	}
	var approved gen.APIToken
	if code := e.Do("POST", "/api/v1/auth/device/approve", map[string]any{"user_code": start.UserCode, "org_id": team, "scopes": []string{"read"}}, &approved); code != http.StatusOK || approved.OrgId != team || approved.CreatedVia != "device" {
		t.Fatalf("approve: %d %+v", code, approved)
	}
	e.Advance(10 * time.Second)
	code, _, got := poll()
	if code != http.StatusOK || got.Token.Id != approved.Id || !strings.HasPrefix(got.Secret, "xb_") {
		t.Fatalf("collect: %d %+v", code, got)
	}
	if _, c, _ := poll(); c != "expired_token" {
		t.Fatalf("collecting twice: %s", c)
	}
	var me gen.User
	if code, _ := e.BearerDo(got.Secret, "GET", "/api/v1/me", nil, &me); code != http.StatusOK || me.Token.OrgId != team || len(me.Token.Scopes) != 1 {
		t.Fatalf("the device token: %d %+v", code, me.Token)
	}

	// A denied login ends with access_denied; approving needs a session.
	if code, _ := e.BearerDo("", "POST", "/api/v1/auth/device", map[string]any{}, &start); code != http.StatusOK {
		t.Fatal(code)
	}
	if code, _ := e.BearerDo(got.Secret, "POST", "/api/v1/auth/device/approve", map[string]any{"user_code": start.UserCode, "org_id": team}, nil); code != http.StatusForbidden {
		t.Fatalf("approving with a token: %d", code)
	}
	if code := e.Do("POST", "/api/v1/auth/device/approve", map[string]any{"user_code": start.UserCode, "approve": false}, nil); code != http.StatusNoContent {
		t.Fatalf("deny: %d", code)
	}
	if _, c, _ := poll(); c != "access_denied" {
		t.Fatalf("after denial: %s", c)
	}
}
