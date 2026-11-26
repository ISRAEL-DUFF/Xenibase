package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
)

// TestReadTokensChangeNothing: a read-scoped token can't make its user
// leave an organisation, or revoke their other tokens; a write token can.
func TestReadTokensChangeNothing(t *testing.T) {
	ts, clk, db := newOAuthServer(t)
	ctx := context.Background()
	b := newBrowser(t, ts.URL)
	b.setup(clk)
	var me gen.User
	if code, _ := b.do("GET", "/api/v1/me", nil, &me); code != 200 {
		t.Fatalf("me: %d", code)
	}
	// Someone else's organisation the user is a member of.
	other, org := uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email, password_hash) VALUES ($1, 'ada@example.com', 'x')`, []any{other}},
		{`INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Acme', 'acme', id FROM quota_plans ORDER BY name LIMIT 1`, []any{org}},
		{`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, []any{org, other, me.Id}},
	} {
		if _, err := db.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	mint := func(name string, scopes ...string) gen.CreatedToken {
		t.Helper()
		var c gen.CreatedToken
		if code, body := b.do("POST", "/api/v1/tokens", map[string]any{"name": name, "org_id": org, "scopes": scopes}, &c); code != http.StatusCreated {
			t.Fatalf("token %s: %d %s", name, code, body)
		}
		return c
	}
	read, write, ops := mint("dashboard", "read"), mint("ci", "read", "write"), mint("ops", "read", "write", "admin")
	bearer := func(token, method, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(raw)
	}

	if code, body := bearer(read.Secret, "DELETE", "/api/v1/tokens/"+ops.Token.Id.String()); code != http.StatusForbidden || !strings.Contains(body, "insufficient_scope") {
		t.Fatalf("a read token revoking another: %d %s", code, body)
	}
	if code, body := bearer(read.Secret, "POST", "/api/v1/orgs/"+org.String()+"/leave"); code != http.StatusForbidden || !strings.Contains(body, "insufficient_scope") {
		t.Fatalf("a read token leaving: %d %s", code, body)
	}
	if code, body := bearer(ops.Secret, "GET", "/api/v1/me"); code != 200 {
		t.Fatalf("the admin token after the read token's tries: %d %s", code, body)
	}
	// With write, both work.
	if code, body := bearer(write.Secret, "DELETE", "/api/v1/tokens/"+ops.Token.Id.String()); code != http.StatusNoContent {
		t.Fatalf("a write token revoking another: %d %s", code, body)
	}
	// Any token may revoke itself (xenibase logout); read is enough.
	if code, body := bearer(read.Secret, "DELETE", "/api/v1/tokens/"+read.Token.Id.String()); code != http.StatusNoContent {
		t.Fatalf("a read token revoking itself: %d %s", code, body)
	}
	if code, body := bearer(write.Secret, "POST", "/api/v1/orgs/"+org.String()+"/leave"); code != http.StatusNoContent {
		t.Fatalf("a write token leaving: %d %s", code, body)
	}
}
