package edge

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/edgeapi"
)

// TestEmailCodesCappedPerAddress: an address gets at most five codes an
// hour and twenty a day, whatever their kind, with or without a user.
func TestEmailCodesCappedPerAddress(t *testing.T) {
	x := newAuthEdge(t, nil)
	for _, addr := range []string{"known@example.com", "nobody@example.com"} {
		if addr == "known@example.com" {
			if r := x.do("POST", "/auth/v1/signup", map[string]any{"email": addr, "password": "correct horse 1"}, ""); r.Code != http.StatusOK {
				t.Fatalf("signup: %d %s", r.Code, r.Body)
			}
			x.later(2 * time.Minute)
		}
		ask := func(i int) authResp {
			t.Helper()
			if i%2 == 0 {
				return x.do("POST", "/auth/v1/recover", map[string]any{"email": addr}, "")
			}
			return x.do("POST", "/auth/v1/signin/otp", map[string]any{"email": addr, "create_user": false}, "")
		}
		n := 0
		if addr == "known@example.com" {
			n = 1 // the sign-up's confirmation
		}
		for ; n < 5; n++ {
			if r := ask(n); r.Code != http.StatusOK {
				t.Fatalf("%s: code %d: %d %s", addr, n+1, r.Code, r.Body)
			}
			x.later(2 * time.Minute)
		}
		r := ask(n)
		if r.Code != http.StatusTooManyRequests || !strings.Contains(r.Body, "this hour") || r.Header.Get("Retry-After") == "" {
			t.Fatalf("%s: a sixth code in an hour: %d %s", addr, r.Code, r.Body)
		}
		x.later(time.Hour)
		for ; n < 20; n++ {
			if r := ask(n); r.Code != http.StatusOK {
				t.Fatalf("%s: code %d: %d %s", addr, n+1, r.Code, r.Body)
			}
			x.later(15 * time.Minute)
		}
		if r := ask(n); r.Code != http.StatusTooManyRequests || !strings.Contains(r.Body, "today") {
			t.Fatalf("%s: a 21st code in a day: %d %s", addr, r.Code, r.Body)
		}
	}
}

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
