package edge

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/totp"
)

// mfaEdge is an edge whose sign-ups are signed in at once.
func mfaEdge(t *testing.T) *authEdge {
	return newAuthEdge(t, func(a *edgeapi.AuthConfig) { a.EmailConfirm = false })
}

// claimOf is a claim of an access token.
func claimOf(t *testing.T, tok, name string) any {
	t.Helper()
	parts := strings.Split(tok, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c[name]
}

// enrolTOTP enrols a TOTP factor with tok and returns its id and secret.
func (x *authEdge) enrolTOTP(tok string) (authResp, string, string) {
	x.t.Helper()
	r := x.do("POST", "/auth/v1/factors", map[string]any{"factor_type": "totp"}, tok)
	if r.Code != http.StatusOK {
		return r, "", ""
	}
	m := r.json(x.t)
	return r, m["id"].(string), m["totp"].(map[string]any)["secret"].(string)
}

// verifyTOTP challenges factor id and answers with the code for at.
func (x *authEdge) verifyTOTP(tok, id, secret string, at time.Time) authResp {
	x.t.Helper()
	ch := x.do("POST", "/auth/v1/factors/"+id+"/challenge", nil, tok)
	if ch.Code != http.StatusOK {
		return ch
	}
	code, err := totp.Code(secret, at)
	if err != nil {
		x.t.Fatal(err)
	}
	return x.do("POST", "/auth/v1/factors/"+id+"/verify", map[string]any{"challenge_id": ch.json(x.t)["id"], "code": code}, tok)
}

// TestAAL1CannotChangeWhatProvesTheUser: once a user has a verified
// factor, a session at aal1 (a password alone) can't enrol another
// factor, change the password or the email, or verify a factor enrolled
// before the user's first was verified; at aal2 it can.
func TestAAL1CannotChangeWhatProvesTheUser(t *testing.T) {
	x := mfaEdge(t)
	const email, pw = "ada@example.com", "correct horse 1"
	accessToken(t, x.do("POST", "/auth/v1/signup", map[string]any{"email": email, "password": pw}, ""))
	// Whoever has the password enrols a factor of their own first.
	r, attackerID, attackerSecret := x.enrolTOTP(accessToken(t, x.signIn(email, pw)))
	if r.Code != http.StatusOK {
		t.Fatalf("enrol with no factors: %d %s", r.Code, r.Body)
	}
	// The owner enrols and verifies theirs.
	owner := accessToken(t, x.signIn(email, pw))
	_, id, secret := x.enrolTOTP(owner)
	v := x.verifyTOTP(owner, id, secret, time.Now())
	aal2 := accessToken(t, v)
	if got := claimOf(t, aal2, "aal"); got != "aal2" {
		t.Fatalf("after verifying: aal %v", got)
	}

	aal1 := accessToken(t, x.signIn(email, pw))
	if got := claimOf(t, aal1, "aal"); got != "aal1" {
		t.Fatalf("a password sign-in: aal %v", got)
	}
	if r, _, _ := x.enrolTOTP(aal1); r.Code != http.StatusForbidden || r.errCode() != "insufficient_aal" {
		t.Fatalf("enrol at aal1: %d %s", r.Code, r.Body)
	}
	if r := x.do("PUT", "/auth/v1/user", map[string]any{"password": "another pass 2"}, aal1); r.Code != http.StatusUnauthorized ||
		r.errCode() != "insufficient_aal" {
		t.Fatalf("password change at aal1: %d %s", r.Code, r.Body)
	}
	if r := x.do("PUT", "/auth/v1/user", map[string]any{"email": "eve@example.com"}, aal1); r.Code != http.StatusUnauthorized ||
		r.errCode() != "insufficient_aal" {
		t.Fatalf("email change at aal1: %d %s", r.Code, r.Body)
	}
	if r := x.do("PUT", "/auth/v1/user", map[string]any{"data": map[string]any{"theme": "dark"}}, aal1); r.Code != http.StatusOK {
		t.Fatalf("metadata at aal1: %d %s", r.Code, r.Body)
	}
	if r := x.verifyTOTP(aal1, attackerID, attackerSecret, time.Now()); r.Code != http.StatusForbidden || r.errCode() != "insufficient_aal" {
		t.Fatalf("verifying a factor enrolled before the owner's, at aal1: %d %s", r.Code, r.Body)
	}

	// At aal2 all of it works; the owner's own factor raises aal1 too.
	if r, _, _ := x.enrolTOTP(aal2); r.Code != http.StatusOK {
		t.Fatalf("enrol at aal2: %d %s", r.Code, r.Body)
	}
	if r := x.do("PUT", "/auth/v1/user", map[string]any{"password": "another pass 2"}, aal2); r.Code != http.StatusOK {
		t.Fatalf("password change at aal2: %d %s", r.Code, r.Body)
	}
	if r := x.verifyTOTP(aal1, id, secret, time.Now().Add(30*time.Second)); r.Code != http.StatusUnauthorized {
		// The password change ended aal1's session.
		t.Fatalf("aal1's session after the password change: %d %s", r.Code, r.Body)
	}
	next := accessToken(t, x.signIn(email, "another pass 2"))
	if r := x.verifyTOTP(next, id, secret, time.Now().Add(30*time.Second)); r.Code != http.StatusOK || claimOf(t, accessToken(t, r), "aal") != "aal2" {
		t.Fatalf("stepping up with the owner's factor: %d %s", r.Code, r.Body)
	}
}
