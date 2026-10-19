package auth_test

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/mail"
)

// inbox captures account email.
type inbox struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (b *inbox) Send(_ context.Context, m mail.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, m)
	return nil
}

var tokenRe = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

// lastToken returns the token in the latest email to addr.
func (b *inbox) lastToken(t *testing.T, addr string) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.msgs) - 1; i >= 0; i-- {
		if b.msgs[i].To[0] == addr {
			if m := tokenRe.FindStringSubmatch(b.msgs[i].Body); m != nil {
				return m[1]
			}
		}
	}
	t.Fatalf("no email with a link to %s", addr)
	return ""
}

func (b *inbox) count(addr string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, m := range b.msgs {
		if m.To[0] == addr {
			n++
		}
	}
	return n
}

func newAccounts(t *testing.T) (*auth.Service, *clock, *inbox, int, string) {
	t.Helper()
	s, c := newService(t, auth.Config{PublicURL: "https://xenibase.test"})
	box := &inbox{}
	s.SetMailer(box)
	ctx := context.Background()
	if err := s.EnsureTerms(ctx); err != nil {
		t.Fatal(err)
	}
	terms, err := s.CurrentTerms(ctx)
	if err != nil || terms.Version != 1 {
		t.Fatalf("default terms: %+v %v", terms, err)
	}
	secret := setupOwner(t, s, c)
	return s, c, box, int(terms.Version), secret
}

// enrol turns two-factor sign-in on for an ordinary account (which signs in
// with a password alone until it does), and returns the secret and recovery codes.
func enrol(t *testing.T, s *auth.Service, c *clock, addr string) (string, []string) {
	t.Helper()
	ctx := context.Background()
	ch, err := s.Login(ctx, addr, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if ch.Enroll != nil || ch.NeedsCode {
		t.Fatalf("an ordinary account signs in without a code until it turns two-factor on: %+v", ch)
	}
	if _, err := s.CompleteLogin(ctx, ch.Token, "", nil, ""); err != nil {
		t.Fatalf("password-only sign-in: %v", err)
	}
	enr, err := s.BeginEnableTOTP(ctx, ch.UserID, password)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	codes, err := s.EnableTOTP(ctx, ch.UserID, enr.Token, code(t, enr.Secret, c))
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if len(codes) != auth.RecoveryCodeCount {
		t.Fatalf("recovery codes: %d", len(codes))
	}
	c.Step()
	return enr.Secret, codes
}

func TestSignupModes(t *testing.T) {
	s, c, box, terms, _ := newAccounts(t)
	ctx := context.Background()
	const alice = "alice@example.com"

	// Invite-only by default.
	if err := s.Signup(ctx, auth.SignupParams{Email: alice, Password: password, TermsVersion: terms}); !errors.Is(err, auth.ErrSignupClosed) {
		t.Fatalf("invite-only signup: %v", err)
	}

	// Open, with a domain allow-list.
	if _, err := s.SetSignupPolicy(ctx, auth.SignupPolicy{Mode: auth.SignupOpen, Domains: []string{"Example.com"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Signup(ctx, auth.SignupParams{Email: "eve@elsewhere.net", Password: password, TermsVersion: terms}); !errors.Is(err, auth.ErrDomainNotAllowed) {
		t.Fatalf("other domain: %v", err)
	}
	if err := s.Signup(ctx, auth.SignupParams{Email: alice, Password: password, TermsVersion: terms + 1}); !errors.Is(err, auth.ErrTermsNotAccepted) {
		t.Fatalf("stale terms version: %v", err)
	}
	if err := s.Signup(ctx, auth.SignupParams{Email: alice, Password: password, Name: "Alice", TermsVersion: terms}); err != nil {
		t.Fatal(err)
	}
	// Nothing works before the address is verified.
	if _, err := s.Login(ctx, alice, password); !errors.Is(err, auth.ErrEmailUnverified) {
		t.Fatalf("unverified login: %v", err)
	}
	// Signing up again with the address says nothing; it sends a notice.
	before := box.count(alice)
	if err := s.Signup(ctx, auth.SignupParams{Email: alice, Password: password, TermsVersion: terms}); err != nil {
		t.Fatal(err)
	}
	if box.count(alice) != before+1 {
		t.Fatal("a repeated signup should email the address")
	}
	if _, err := s.VerifyEmail(ctx, "bogus"); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Fatalf("bogus token: %v", err)
	}
	if err := s.ResendVerification(ctx, alice); err != nil {
		t.Fatal(err)
	}
	u, err := s.VerifyEmail(ctx, box.lastToken(t, alice))
	if err != nil || u.EmailVerifiedAt == nil {
		t.Fatalf("verify: %+v %v", u, err)
	}
	enrol(t, s, c, alice)

	// Approval mode: verified, but waiting for the platform admin.
	if _, err := s.SetSignupPolicy(ctx, auth.SignupPolicy{Mode: auth.SignupApproval}); err != nil {
		t.Fatal(err)
	}
	const bob = "bob@example.org"
	if err := s.Signup(ctx, auth.SignupParams{Email: bob, Password: password, TermsVersion: terms}); err != nil {
		t.Fatal(err)
	}
	ub, err := s.VerifyEmail(ctx, box.lastToken(t, bob))
	if err != nil || ub.ApprovedAt != nil {
		t.Fatalf("verify bob: %+v %v", ub, err)
	}
	if _, err := s.Login(ctx, bob, password); !errors.Is(err, auth.ErrPendingApproval) {
		t.Fatalf("unapproved login: %v", err)
	}
	if _, err := s.ApproveUser(ctx, ub.ID); err != nil {
		t.Fatal(err)
	}
	enrol(t, s, c, bob)
}

func TestRecoveryCodes(t *testing.T) {
	s, c, box, terms, _ := newAccounts(t)
	ctx := context.Background()
	if _, err := s.SetSignupPolicy(ctx, auth.SignupPolicy{Mode: auth.SignupOpen}); err != nil {
		t.Fatal(err)
	}
	const carol = "carol@example.com"
	if err := s.Signup(ctx, auth.SignupParams{Email: carol, Password: password, TermsVersion: terms}); err != nil {
		t.Fatal(err)
	}
	u, err := s.VerifyEmail(ctx, box.lastToken(t, carol))
	if err != nil {
		t.Fatal(err)
	}
	_, codes := enrol(t, s, c, carol)

	ch, err := s.Login(ctx, carol, password)
	if err != nil || ch.Enroll != nil {
		t.Fatalf("second login: %+v %v", ch, err)
	}
	if _, err := s.CompleteLogin(ctx, ch.Token, codes[3], nil, ""); err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	if n, _ := s.RecoveryCodesLeft(ctx, u.ID); n != auth.RecoveryCodeCount-1 {
		t.Fatalf("codes left: %d", n)
	}
	// A recovery code works once.
	ch, _ = s.Login(ctx, carol, password)
	if _, err := s.CompleteLogin(ctx, ch.Token, codes[3], nil, ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("reused recovery code: %v", err)
	}
	fresh, err := s.RegenerateRecoveryCodes(ctx, u.ID)
	if err != nil || len(fresh) != auth.RecoveryCodeCount {
		t.Fatal(err)
	}
	ch, _ = s.Login(ctx, carol, password)
	if _, err := s.CompleteLogin(ctx, ch.Token, codes[0], nil, ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old code after regenerating: %v", err)
	}

	// The platform admin resets 2FA: carol signs in with a password until she turns it on again.
	if err := s.ResetTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	c.Step()
	enrol(t, s, c, carol)
}

func TestPasswordReset(t *testing.T) {
	s, c, box, _, secret := newAccounts(t)
	ctx := context.Background()
	tok, _ := login(t, s, c, secret)

	if err := s.RequestPasswordReset(ctx, "nobody@example.com"); err != nil {
		t.Fatalf("unknown address: %v", err)
	}
	if err := s.RequestPasswordReset(ctx, email); err != nil {
		t.Fatal(err)
	}
	link := box.lastToken(t, email)
	const next = "an entirely new passphrase"
	if err := s.ResetPassword(ctx, link, "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	if err := s.ResetPassword(ctx, link, next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, tok); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("a reset should end every session: %v", err)
	}
	if err := s.ResetPassword(ctx, link, next); !errors.Is(err, auth.ErrTokenInvalid) {
		t.Fatalf("reused link: %v", err)
	}
	if _, err := s.Login(ctx, email, password); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old password: %v", err)
	}
	if _, err := s.Login(ctx, email, next); err != nil {
		t.Fatalf("new password: %v", err)
	}
}

func TestTermsVersions(t *testing.T) {
	s, c, _, _, secret := newAccounts(t)
	ctx := context.Background()
	_, sess := login(t, s, c, secret)
	if pending, _, err := s.TermsOutstanding(ctx, sess.UserID); err != nil || pending {
		t.Fatalf("setup accepted version 1: %v %v", pending, err)
	}
	v2, err := s.PublishTerms(ctx, "# Terms v2", "# Privacy v2", "", sess.UserID)
	if err != nil || v2.Version != 2 {
		t.Fatalf("publish: %+v %v", v2, err)
	}
	pending, v, err := s.TermsOutstanding(ctx, sess.UserID)
	if err != nil || !pending || v != 2 {
		t.Fatalf("after publishing: %v %d %v", pending, v, err)
	}
	if err := s.AcceptTerms(ctx, sess.UserID, 1, nil); !errors.Is(err, auth.ErrTermsNotAccepted) {
		t.Fatalf("accepting an old version: %v", err)
	}
	if err := s.AcceptTerms(ctx, sess.UserID, 2, nil); err != nil {
		t.Fatal(err)
	}
	if pending, _, _ := s.TermsOutstanding(ctx, sess.UserID); pending {
		t.Fatal("still pending after accepting")
	}
}

func TestTwoFactorIsOptIn(t *testing.T) {
	s, c, box, terms, ownerSecret := newAccounts(t)
	ctx := context.Background()
	if _, err := s.SetSignupPolicy(ctx, auth.SignupPolicy{Mode: auth.SignupOpen}); err != nil {
		t.Fatal(err)
	}
	const dana = "dana@example.com"
	if err := s.Signup(ctx, auth.SignupParams{Email: dana, Password: password, TermsVersion: terms}); err != nil {
		t.Fatal(err)
	}
	u, err := s.VerifyEmail(ctx, box.lastToken(t, dana))
	if err != nil {
		t.Fatal(err)
	}

	// Off by default: the password alone signs in.
	if st, err := s.TOTPStatus(ctx, u.ID); err != nil || st.Enabled || st.Required {
		t.Fatalf("status of a new account: %+v %v", st, err)
	}
	ch, err := s.Login(ctx, dana, password)
	if err != nil || ch.NeedsCode || ch.Enroll != nil {
		t.Fatalf("login: %+v %v", ch, err)
	}
	in, err := s.CompleteLogin(ctx, ch.Token, "", nil, "")
	if err != nil || len(in.RecoveryCodes) != 0 {
		t.Fatalf("password-only sign-in: %+v %v", in, err)
	}
	// With no second factor, step-up asks for the password only, and there are no recovery codes to replace.
	if err := s.Reauthenticate(ctx, in.Session, password, ""); err != nil {
		t.Fatalf("reauth with the password: %v", err)
	}
	if _, err := s.RegenerateRecoveryCodes(ctx, u.ID); !errors.Is(err, auth.ErrTOTPOff) {
		t.Fatalf("recovery codes while off: %v", err)
	}
	if err := s.DisableTOTP(ctx, u.ID, password, "123456"); !errors.Is(err, auth.ErrTOTPOff) {
		t.Fatalf("disable while off: %v", err)
	}

	// Turning it on needs the password, and a code from the authenticator.
	if _, err := s.BeginEnableTOTP(ctx, u.ID, "not the password at all"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("setup with a wrong password: %v", err)
	}
	enr, err := s.BeginEnableTOTP(ctx, u.ID, password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnableTOTP(ctx, u.ID, enr.Token, "000000"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("enable with a wrong code: %v", err)
	}
	if _, err := s.EnableTOTP(ctx, uuid.New(), enr.Token, code(t, enr.Secret, c)); !errors.Is(err, auth.ErrChallengeExpired) {
		t.Fatalf("someone else's setup token: %v", err)
	}
	codes, err := s.EnableTOTP(ctx, u.ID, enr.Token, code(t, enr.Secret, c))
	if err != nil || len(codes) != auth.RecoveryCodeCount {
		t.Fatalf("enable: %v", err)
	}
	if _, err := s.EnableTOTP(ctx, u.ID, enr.Token, code(t, enr.Secret, c)); err == nil {
		t.Fatal("a setup token works once")
	}
	if _, err := s.BeginEnableTOTP(ctx, u.ID, password); !errors.Is(err, auth.ErrTOTPOn) {
		t.Fatalf("setup while on: %v", err)
	}
	if box.count(dana) < 2 { // verification, then a notice that it was turned on
		t.Fatalf("no notice that two-factor was turned on")
	}

	// On: sign-in needs the code, and a setup token is not a login challenge.
	c.Step()
	ch, err = s.Login(ctx, dana, password)
	if err != nil || !ch.NeedsCode {
		t.Fatalf("login with two-factor on: %+v %v", ch, err)
	}
	if _, err := s.CompleteLogin(ctx, ch.Token, "", nil, ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("no code with two-factor on: %v", err)
	}
	ch, _ = s.Login(ctx, dana, password)
	if _, err := s.CompleteLogin(ctx, ch.Token, code(t, enr.Secret, c), nil, ""); err != nil {
		t.Fatalf("sign-in with the code: %v", err)
	}
	c.Step()

	// Turning it off needs the password and a code.
	if err := s.DisableTOTP(ctx, u.ID, "not the password at all", code(t, enr.Secret, c)); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("disable with a wrong password: %v", err)
	}
	if err := s.DisableTOTP(ctx, u.ID, password, "000000"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("disable with a wrong code: %v", err)
	}
	if err := s.DisableTOTP(ctx, u.ID, password, codes[0]); err != nil { // a recovery code will do
		t.Fatalf("disable: %v", err)
	}
	if st, _ := s.TOTPStatus(ctx, u.ID); st.Enabled {
		t.Fatal("still on")
	}
	ch, err = s.Login(ctx, dana, password)
	if err != nil || ch.NeedsCode {
		t.Fatalf("login after turning it off: %+v %v", ch, err)
	}

	// Platform staff cannot turn it off, and always enrol.
	owner, err := s.Login(ctx, email, password)
	if err != nil || !owner.NeedsCode {
		t.Fatalf("the platform admin: %+v %v", owner, err)
	}
	if err := s.DisableTOTP(ctx, owner.UserID, password, code(t, ownerSecret, c)); !errors.Is(err, auth.ErrTOTPRequired) {
		t.Fatalf("a platform admin turning it off: %v", err)
	}
	if st, _ := s.TOTPStatus(ctx, owner.UserID); !st.Enabled || !st.Required {
		t.Fatalf("admin status: %+v", st)
	}
	if err := s.ResetTOTP(ctx, owner.UserID); err != nil {
		t.Fatal(err)
	}
	if ch, err := s.Login(ctx, email, password); err != nil || ch.Enroll == nil {
		t.Fatalf("a platform admin whose two-factor was reset enrols at the next sign-in: %+v %v", ch, err)
	}
}
