package edge

import (
	"net/http"
	"testing"

	"github.com/israel-duff/xenibase/internal/edgeapi"
)

// TestCodeLimitsDontRevealUsers: asking twice in a minute for a reset, a
// sign-in code without creating the user, or a confirmation answers the
// same for an address (or number) with a user as for one without.
func TestCodeLimitsDontRevealUsers(t *testing.T) {
	x := newAuthEdge(t, func(a *edgeapi.AuthConfig) {
		a.EmailConfirm = false
		a.PhoneChannels = []string{edgeapi.ChannelSMS}
		a.PhoneConfirm = false
	})
	if r := x.do("POST", "/auth/v1/signup", map[string]any{"email": "known@example.com", "password": "correct horse 1"}, ""); r.Code != http.StatusOK {
		t.Fatalf("signup: %d %s", r.Code, r.Body)
	}
	if r := x.do("POST", "/auth/v1/signup", map[string]any{"phone": "+2348031234567", "password": "correct horse 1"}, ""); r.Code != http.StatusOK {
		t.Fatalf("phone signup: %d %s", r.Code, r.Body)
	}
	twice := func(path string, body func(string) map[string]any, target string) [2]int {
		t.Helper()
		var out [2]int
		for i := range out {
			r := x.do("POST", path, body(target), "")
			out[i] = r.Code
		}
		return out
	}
	for _, c := range []struct {
		path           string
		body           func(string) map[string]any
		known, unknown string
	}{
		{"/auth/v1/recover", func(a string) map[string]any { return map[string]any{"email": a} }, "known@example.com", "nobody@example.com"},
		{"/auth/v1/signin/otp", func(a string) map[string]any { return map[string]any{"email": a, "create_user": false} },
			"known@example.com", "nobody2@example.com"},
		{"/auth/v1/resend", func(a string) map[string]any { return map[string]any{"type": "signup", "email": a} },
			"known@example.com", "nobody3@example.com"},
		{"/auth/v1/signin/otp", func(a string) map[string]any { return map[string]any{"phone": a, "create_user": false} },
			"+2348031234567", "+2348031234500"},
	} {
		k, u := twice(c.path, c.body, c.known), twice(c.path, c.body, c.unknown)
		if k != u || k[1] != http.StatusTooManyRequests {
			t.Errorf("%s twice: %v with a user, %v without (want 200 then 429 for both)", c.path, k, u)
		}
	}
}
