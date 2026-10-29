package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/edge"
	"github.com/israel-duff/xenibase/internal/totp"
)

// TestAuthCodeBruteForce is V4.1-M11's code brute-force check (V4.1 §12.3):
// five wrong codes use up an email code, a phone code and an MFA phone
// challenge, so the right code is refused on the sixth try; a used-up code
// still holds the address to one code a minute; the per-IP limit stops
// code requests; and a TOTP code can't be used twice.
func TestAuthCodeBruteForce(t *testing.T) {
	sp := newStorageProject(t, "guarded", false)
	e := sp.e
	if code := e.Do("PATCH", "/api/v1/projects/"+sp.pid.String()+"/auth/config", gen.AuthConfigUpdate{Settings: &gen.AuthSettings{
		PhoneChannels: &[]gen.AuthSettingsPhoneChannels{gen.AuthSettingsPhoneChannelsSms}, MfaPhone: ptr(true),
	}}, nil); code != http.StatusOK {
		t.Fatalf("auth config: %d", code)
	}
	pub := authAPI{t: t, ed: sp.ed, ref: sp.ref, key: sp.anon.key}
	box := &mailbox{e: e, seen: map[string]int{}}
	wrongFor := func(right string) string {
		if right == "000000" {
			return "111111"
		}
		return "000000"
	}
	guess := func(what string, wantStatus int, wantCode string, try func(code string) authResult, right string) {
		t.Helper()
		for i := 1; i <= 5; i++ {
			if r := try(wrongFor(right)); r.Code != wantStatus || r.Error.Code != wantCode {
				t.Fatalf("%s: wrong code %d: %d %s", what, i, r.Code, r.Body)
			}
		}
		if r := try(right); r.Code != wantStatus || r.Error.Code != wantCode {
			t.Fatalf("%s: the right code after five wrong ones: %d %s", what, r.Code, r.Body)
		}
	}

	// ---- An email code ---------------------------------------------------------
	const alice = "alice@example.com"
	if r := pub.post("/auth/v1/signin/otp", fmt.Sprintf(`{"email":%q}`, alice)); r.Code != http.StatusOK {
		t.Fatalf("email code: %d %s", r.Code, r.Body)
	}
	code, _ := box.next(t, alice)
	guess("email", http.StatusForbidden, "otp_expired", func(c string) authResult {
		return pub.post("/auth/v1/verify", fmt.Sprintf(`{"type":"email","email":%q,"token":%q}`, alice, c))
	}, code)
	// The used-up code still counts: no fresh code (and five more guesses)
	// within the minute.
	if r := pub.post("/auth/v1/signin/otp", fmt.Sprintf(`{"email":%q}`, alice)); r.Code != http.StatusTooManyRequests ||
		r.Error.Code != "over_email_send_rate_limit" {
		t.Fatalf("a new email code at once after using one up: %d %s", r.Code, r.Body)
	}

	// ---- A phone code ----------------------------------------------------------
	const phone = "+2348031230001"
	var sent authResult
	waitFor(t, 30*time.Second, "phone sign-in reaching the edge", func() bool {
		sent = pub.post("/auth/v1/signin/otp", fmt.Sprintf(`{"phone":%q,"channel":"sms"}`, phone))
		return sent.Error.Code != "phone_provider_disabled"
	})
	if sent.Code != http.StatusOK {
		t.Fatalf("phone code: %d %s", sent.Code, sent.Body)
	}
	code = phoneCode(t, e, "sms", phone, 0)
	guess("phone", http.StatusForbidden, "otp_expired", func(c string) authResult {
		return pub.post("/auth/v1/verify", fmt.Sprintf(`{"type":"sms","phone":%q,"token":%q}`, phone, c))
	}, code)
	if r := pub.post("/auth/v1/signin/otp", fmt.Sprintf(`{"phone":%q,"channel":"sms"}`, phone)); r.Code != http.StatusTooManyRequests {
		t.Fatalf("a new phone code at once after using one up: %d %s", r.Code, r.Body)
	}

	// ---- An MFA phone challenge --------------------------------------------------
	session := pub.post("/auth/v1/signin/password", fmt.Sprintf(`{"email":%q,"password":"password-123456"}`, alice))
	if session.Code != http.StatusOK {
		t.Fatalf("sign in: %d %s", session.Code, session.Body)
	}
	token := session.Session.AccessToken
	const mfaPhone = "+2348031230002"
	r := pub.call("POST", "/auth/v1/factors", fmt.Sprintf(`{"factor_type":"phone","phone":%q}`, mfaPhone), token)
	var factor struct {
		ID string `json:"id"`
	}
	if r.Code != http.StatusOK || json.Unmarshal([]byte(r.Body), &factor) != nil || factor.ID == "" {
		t.Fatalf("enrol phone: %d %s", r.Code, r.Body)
	}
	challenge := func(id string) string {
		t.Helper()
		r := pub.call("POST", "/auth/v1/factors/"+id+"/challenge", `{}`, token)
		var ch struct {
			ID string `json:"id"`
		}
		if r.Code != http.StatusOK || json.Unmarshal([]byte(r.Body), &ch) != nil || ch.ID == "" {
			t.Fatalf("challenge: %d %s", r.Code, r.Body)
		}
		return ch.ID
	}
	chID := challenge(factor.ID)
	code = phoneCode(t, e, "sms", mfaPhone, 0)
	// Five wrong codes in a row also lock the factor, across challenges.
	mfaTry := func(c string) authResult {
		return pub.call("POST", "/auth/v1/factors/"+factor.ID+"/verify", fmt.Sprintf(`{"challenge_id":%q,"code":%q}`, chID, c), token)
	}
	for i := 1; i <= 5; i++ {
		if r := mfaTry(wrongFor(code)); r.Code != http.StatusUnprocessableEntity || r.Error.Code != "mfa_verification_failed" {
			t.Fatalf("mfa phone: wrong code %d: %d %s", i, r.Code, r.Body)
		}
	}
	if r := mfaTry(code); r.Code != http.StatusTooManyRequests || r.Error.Code != "mfa_factor_locked" {
		t.Fatalf("mfa phone: the right code after five wrong ones: %d %s", r.Code, r.Body)
	}
	if r := pub.call("POST", "/auth/v1/factors/"+factor.ID+"/challenge", `{}`, token); r.Code != http.StatusTooManyRequests {
		t.Fatalf("mfa phone: a new challenge for a locked factor: %d %s", r.Code, r.Body)
	}

	// ---- A TOTP code works once ----------------------------------------------------
	r = pub.call("POST", "/auth/v1/factors", `{"factor_type":"totp","friendly_name":"app"}`, token)
	var enrolled struct {
		ID   string `json:"id"`
		TOTP struct {
			Secret string `json:"secret"`
		} `json:"totp"`
	}
	if r.Code != http.StatusOK || json.Unmarshal([]byte(r.Body), &enrolled) != nil || enrolled.TOTP.Secret == "" {
		t.Fatalf("enrol totp: %d %s", r.Code, r.Body)
	}
	// Start on a fresh 30-second step so both tries fall within it.
	if wait := 30 - time.Now().Unix()%30; wait < 5 {
		time.Sleep(time.Duration(wait+1) * time.Second)
	}
	totpCode, err := totp.Code(enrolled.TOTP.Secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	verify := func() authResult {
		return pub.call("POST", "/auth/v1/factors/"+enrolled.ID+"/verify",
			fmt.Sprintf(`{"challenge_id":%q,"code":%q}`, challenge(enrolled.ID), totpCode), token)
	}
	if r := verify(); r.Code != http.StatusOK {
		t.Fatalf("totp: %d %s", r.Code, r.Body)
	}
	if r := verify(); r.Code != http.StatusUnprocessableEntity || r.Error.Code != "mfa_verification_failed" {
		t.Fatalf("the same totp code again: %d %s", r.Code, r.Body)
	}

	// ---- The per-IP limit on asking for codes (10 a minute) -------------------------
	strict := e.StartEdge(func(c *edge.Config) { c.AuthRateScale = 1 })
	strictAPI := authAPI{t: t, ed: strict, ref: sp.ref, key: sp.anon.key}
	waitFor(t, 30*time.Second, "the second edge", func() bool {
		return strictAPI.call("GET", "/auth/v1/settings", "", "").Code == http.StatusOK
	})
	limited := 0
	for i := 0; i < 12; i++ {
		r := strictAPI.post("/auth/v1/signin/otp", fmt.Sprintf(`{"email":"nobody%d@example.com","create_user":false}`, i))
		if r.Code == http.StatusTooManyRequests && r.Error.Code == "over_request_rate_limit" {
			limited++
		}
	}
	if limited < 2 {
		t.Fatalf("12 code requests from one address in a few seconds: %d refused", limited)
	}
}

// TestPasswordChangeEndsOtherSessions (ASVS 3.3.3, V4.1 §12.2): changing
// the password signs out the user's other sessions, not the one that
// changed it.
func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	sp := newStorageProject(t, "sessions", false)
	pub := authAPI{t: t, ed: sp.ed, ref: sp.ref, key: sp.anon.key}
	signIn := func(pw string) authResult {
		t.Helper()
		r := pub.post("/auth/v1/signin/password", fmt.Sprintf(`{"email":"alice@example.com","password":%q}`, pw))
		if r.Code != http.StatusOK {
			t.Fatalf("sign in: %d %s", r.Code, r.Body)
		}
		return r
	}
	laptop, phone := signIn("password-123456"), signIn("password-123456")
	if r := pub.call("PUT", "/auth/v1/user", `{"password":"a-new-password-789"}`, laptop.Session.AccessToken); r.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", r.Code, r.Body)
	}
	if r := pub.post("/auth/v1/token?grant_type=refresh_token", fmt.Sprintf(`{"refresh_token":%q}`, phone.Session.RefreshToken)); r.Code == http.StatusOK {
		t.Fatalf("the other session refreshed after the password change: %s", r.Body)
	}
	if r := pub.call("GET", "/auth/v1/user", "", phone.Session.AccessToken); r.Code != http.StatusUnauthorized {
		t.Fatalf("the other session's access token after the change: %d", r.Code)
	}
	if r := pub.post("/auth/v1/token?grant_type=refresh_token", fmt.Sprintf(`{"refresh_token":%q}`, laptop.Session.RefreshToken)); r.Code != http.StatusOK {
		t.Fatalf("the session that changed it: %d %s", r.Code, r.Body)
	}
	signIn("a-new-password-789")
}
