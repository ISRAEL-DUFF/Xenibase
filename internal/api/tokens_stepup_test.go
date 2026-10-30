package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/auth"
)

// TestTokensNeedARecentStepUp: a session creates a token, or approves a
// device login, only after a recent step-up, so a stolen cookie can't mint
// a long-lived token; denying a device login needs none.
func TestTokensNeedARecentStepUp(t *testing.T) {
	ts, clk, _ := newOAuthServer(t)
	b := newBrowser(t, ts.URL)
	secret := b.setup(clk)
	var me gen.User
	if code, body := b.do("GET", "/api/v1/me", nil, &me); code != 200 {
		t.Fatalf("me: %d %s", code, body)
	}
	var orgsList gen.OrgList
	if code, _ := b.do("GET", "/api/v1/orgs", nil, &orgsList); code != 200 || len(orgsList.Items) == 0 {
		t.Fatalf("orgs: %d", code)
	}
	org := orgsList.Items[0].Id
	newToken := map[string]any{"name": "laptop", "org_id": org, "scopes": []string{"read", "write"}}

	// Fresh from sign-in, it works.
	if code, body := b.do("POST", "/api/v1/tokens", newToken, nil); code != http.StatusCreated {
		t.Fatalf("token from a fresh session: %d %s", code, body)
	}

	clk.mu.Lock()
	clk.t = clk.t.Add(2 * time.Hour)
	clk.mu.Unlock()
	if code, body := b.do("POST", "/api/v1/tokens", newToken, nil); code != http.StatusForbidden || !strings.Contains(body, "reauth_required") {
		t.Fatalf("token from a stale session: %d %s", code, body)
	}
	device := func() string {
		var d gen.DeviceAuthorization
		if code, body := b.do("POST", "/api/v1/auth/device", map[string]any{"client_name": "xenibase CLI"}, &d); code != 200 {
			t.Fatalf("device start: %d %s", code, body)
		}
		return d.UserCode
	}
	code1 := device()
	if code, body := b.do("POST", "/api/v1/auth/device/approve", map[string]any{"user_code": code1, "org_id": org}, nil); code != http.StatusForbidden || !strings.Contains(body, "reauth_required") {
		t.Fatalf("device approval from a stale session: %d %s", code, body)
	}
	if code, body := b.do("POST", "/api/v1/auth/device/approve", map[string]any{"user_code": code1, "approve": false}, nil); code != http.StatusNoContent {
		t.Fatalf("denying needs no step-up: %d %s", code, body)
	}

	// After a step-up both work again.
	code, _ := auth.TOTPCode(secret, clk.Now())
	if c, body := b.do("POST", "/api/v1/auth/reauth", map[string]string{"password": ownerPassword, "code": code}, nil); c != http.StatusNoContent {
		t.Fatalf("reauth: %d %s", c, body)
	}
	if c, body := b.do("POST", "/api/v1/tokens", newToken, nil); c != http.StatusCreated {
		t.Fatalf("token after step-up: %d %s", c, body)
	}
	if c, body := b.do("POST", "/api/v1/auth/device/approve", map[string]any{"user_code": device(), "org_id": org}, nil); c != http.StatusOK {
		t.Fatalf("device approval after step-up: %d %s", c, body)
	}
}
