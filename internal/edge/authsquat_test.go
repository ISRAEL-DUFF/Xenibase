package edge

import (
	"net/http"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/edgeapi"
)

// TestSquatterLosesPasswordToOwnersSignup: someone signs up first with an
// address that isn't theirs. When the owner signs up and confirms, the
// account has the owner's password, not the squatter's: through a sign-up
// sent again, a squatter's sign-up within the minute, an invitation, a
// sign-up link from the admin API and phone sign-up. A confirmation sent
// again (/resend) keeps the password of the sign-up it repeats.
func TestSquatterLosesPasswordToOwnersSignup(t *testing.T) {
	x := newAuthEdge(t, func(a *edgeapi.AuthConfig) { a.PhoneChannels = []string{edgeapi.ChannelSMS} })
	const squat, owner = "squatter-pass-1", "owner-pass-12"
	signup := func(body map[string]any) {
		t.Helper()
		if r := x.do("POST", "/auth/v1/signup", body, ""); r.Code != http.StatusOK {
			t.Fatalf("signup: %d %s", r.Code, r.Body)
		}
	}
	verify := func(typ, to, code string) {
		t.Helper()
		body := map[string]any{"type": typ, "email": to, "token": code}
		if typ == "sms" {
			body = map[string]any{"type": typ, "phone": to, "token": code}
		}
		if r := x.do("POST", "/auth/v1/verify", body, ""); r.Code != http.StatusOK {
			t.Fatalf("verify %s: %d %s", typ, r.Code, r.Body)
		}
	}
	signsIn := func(addr, pw string, want int) {
		t.Helper()
		body := map[string]any{"email": addr, "password": pw}
		if addr[0] == '+' {
			body = map[string]any{"phone": addr, "password": pw}
		}
		if r := x.do("POST", "/auth/v1/token?grant_type=password", body, ""); r.Code != want {
			t.Fatalf("%s with %q: %d, want %d: %s", addr, pw, r.Code, want, r.Body)
		}
	}

	// The owner signs up a minute later: a new confirmation, which keeps
	// the owner's password.
	signup(map[string]any{"email": "a@example.com", "password": squat})
	x.later(2 * time.Minute)
	signup(map[string]any{"email": "a@example.com", "password": owner})
	m, _ := x.last("a@example.com")
	verify("signup", "a@example.com", m.Code)
	signsIn("a@example.com", squat, http.StatusBadRequest)
	signsIn("a@example.com", owner, http.StatusOK)

	// The owner signs up within the squatter's minute (no new email) and
	// uses the squatter's confirmation: no password is kept.
	signup(map[string]any{"email": "b@example.com", "password": squat})
	signup(map[string]any{"email": "b@example.com", "password": owner})
	if n := x.sentTo("b@example.com"); n != 1 {
		t.Fatalf("%d emails within the minute, want 1", n)
	}
	m, _ = x.last("b@example.com")
	verify("signup", "b@example.com", m.Code)
	signsIn("b@example.com", squat, http.StatusBadRequest)
	signsIn("b@example.com", owner, http.StatusBadRequest)

	// The squatter signs up again after the owner: the owner's link keeps
	// no password.
	signup(map[string]any{"email": "c@example.com", "password": owner})
	signup(map[string]any{"email": "c@example.com", "password": squat})
	m, _ = x.last("c@example.com")
	verify("signup", "c@example.com", m.Code)
	signsIn("c@example.com", squat, http.StatusBadRequest)

	// /resend repeats the owner's own sign-up: its code keeps the password.
	signup(map[string]any{"email": "d@example.com", "password": owner})
	x.later(2 * time.Minute)
	if r := x.do("POST", "/auth/v1/resend", map[string]any{"type": "signup", "email": "d@example.com"}, ""); r.Code != http.StatusOK {
		t.Fatalf("resend: %d %s", r.Code, r.Body)
	}
	if n := x.sentTo("d@example.com"); n != 2 {
		t.Fatalf("%d emails, want 2", n)
	}
	m, _ = x.last("d@example.com")
	verify("signup", "d@example.com", m.Code)
	signsIn("d@example.com", owner, http.StatusOK)

	// An invitation of a squatted address clears the squatter's password.
	signup(map[string]any{"email": "e@example.com", "password": squat})
	if r := x.do("POST", "/auth/v1/admin/invite", map[string]any{"email": "e@example.com"}, ""); r.Code != http.StatusOK {
		t.Fatalf("invite: %d %s", r.Code, r.Body)
	}
	m, _ = x.last("e@example.com")
	verify("invite", "e@example.com", m.Code)
	signsIn("e@example.com", squat, http.StatusBadRequest)

	// A sign-up link from the admin API sets its own password.
	signup(map[string]any{"email": "f@example.com", "password": squat})
	r := x.do("POST", "/auth/v1/admin/generate-link", map[string]any{"type": "signup", "email": "f@example.com", "password": owner}, "")
	if r.Code != http.StatusOK {
		t.Fatalf("generate-link: %d %s", r.Code, r.Body)
	}
	verify("signup", "f@example.com", r.json(t)["email_otp"].(string))
	signsIn("f@example.com", squat, http.StatusBadRequest)
	signsIn("f@example.com", owner, http.StatusOK)

	// Phone sign-up, the same.
	const phone = "+2348031234567"
	signup(map[string]any{"phone": phone, "password": squat})
	x.later(2 * time.Minute)
	signup(map[string]any{"phone": phone, "password": owner})
	m, _ = x.last(phone)
	verify("sms", phone, m.Code)
	signsIn(phone, squat, http.StatusBadRequest)
	signsIn(phone, owner, http.StatusOK)
}
