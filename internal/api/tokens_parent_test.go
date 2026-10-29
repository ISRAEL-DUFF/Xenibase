package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/api/gen"
)

// TestTokenMadeByATokenGoesWithIt: POST /tokens with a token records it as
// the new token's parent, and revoking the parent revokes the child.
func TestTokenMadeByATokenGoesWithIt(t *testing.T) {
	ts, clk, _ := newOAuthServer(t)
	b := newBrowser(t, ts.URL)
	b.setup(clk)
	var orgsList gen.OrgList
	if code, _ := b.do("GET", "/api/v1/orgs", nil, &orgsList); code != 200 || len(orgsList.Items) == 0 {
		t.Fatalf("orgs: %d", code)
	}
	org := orgsList.Items[0].Id.String()
	var parent gen.CreatedToken
	if code, body := b.do("POST", "/api/v1/tokens", map[string]any{"name": "ci", "org_id": org, "scopes": []string{"read", "write"}, "expires_in_days": 30}, &parent); code != http.StatusCreated {
		t.Fatalf("parent: %d %s", code, body)
	}
	bearer := func(token, method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
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
	code, body := bearer(parent.Secret, "POST", "/api/v1/tokens", `{"name":"child","org_id":"`+org+`","scopes":["read"],"expires_in_days":7}`)
	if code != http.StatusCreated {
		t.Fatalf("child: %d %s", code, body)
	}
	secret := body[strings.Index(body, `"secret":"`)+10:]
	secret = secret[:strings.Index(secret, `"`)]
	if code, body := bearer(secret, "GET", "/api/v1/me", ""); code != 200 {
		t.Fatalf("child works: %d %s", code, body)
	}
	if code, body := b.do("DELETE", "/api/v1/tokens/"+parent.Token.Id.String(), nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke the parent: %d %s", code, body)
	}
	if code, body := bearer(secret, "GET", "/api/v1/me", ""); code != http.StatusUnauthorized {
		t.Fatalf("the child after its parent was revoked: %d %s", code, body)
	}
}
