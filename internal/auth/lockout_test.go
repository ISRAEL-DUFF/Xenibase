package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/auth"
)

func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// lockedFor returns how long the lock behind err has left, failing the
// test when err is not ErrLocked.
func lockedFor(t *testing.T, err error, c *clock, what string) time.Duration {
	t.Helper()
	until, ok := auth.LockedUntil(err)
	if !errors.Is(err, auth.ErrLocked) || !ok {
		t.Fatalf("%s: want locked, got %v", what, err)
	}
	return until.Sub(c.Now()).Round(time.Second)
}

func (b *inbox) subjects(addr string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, m := range b.msgs {
		if m.To[0] == addr {
			out = append(out, m.Subject)
		}
	}
	return out
}

// TestLockoutDoublesAndSaysNothing: the lock doubles with each further
// failure up to the maximum; an address without an account is locked
// exactly as one with an account is, so neither the answers nor the lock
// tell them apart; and the owner is emailed once when it locks.
func TestLockoutDoublesAndSaysNothing(t *testing.T) {
	s, c := newService(t, auth.Config{LockoutThreshold: 3, LockoutDuration: time.Minute, MaxLockout: 4 * time.Minute})
	box := &inbox{}
	s.SetMailer(box)
	ctx := context.Background()
	secret := setupOwner(t, s, c)

	for _, addr := range []string{email, "nobody@example.com"} {
		for i := range 2 {
			if _, err := s.Login(ctx, addr, "wrong password!!"); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatalf("%s attempt %d: %v", addr, i, err)
			}
		}
		_, err := s.Login(ctx, addr, "wrong password!!")
		if d := lockedFor(t, err, c, addr+" third failure"); d != time.Minute {
			t.Fatalf("%s: first lock %v, want 1m", addr, d)
		}
		// While locked, every password gets the same answer, the right one
		// included.
		_, err = s.Login(ctx, addr, password)
		lockedFor(t, err, c, addr+" right password while locked")
	}

	// Each failure after the lock ends locks it for twice as long, to the
	// maximum, for both.
	for _, want := range []time.Duration{2 * time.Minute, 4 * time.Minute, 4 * time.Minute} {
		c.advance(5 * time.Minute)
		for _, addr := range []string{email, "nobody@example.com"} {
			_, err := s.Login(ctx, addr, "wrong password!!")
			if d := lockedFor(t, err, c, addr); d != want {
				t.Fatalf("%s: lock %v, want %v", addr, d, want)
			}
		}
	}

	// The right password works once the lock ends, and starts the count again.
	c.advance(5 * time.Minute)
	login(t, s, c, secret)
	if _, err := s.Login(ctx, email, "wrong password!!"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("after a good sign-in: %v", err)
	}

	// One email for the four locks within the hour, and none for the
	// address without an account.
	deadline := time.Now().Add(5 * time.Second)
	for len(box.subjects(email)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := box.subjects(email); len(got) != 1 || !strings.Contains(got[0], "locked") {
		t.Fatalf("lockout emails: %q", got)
	}
	if n := box.count("nobody@example.com"); n != 0 {
		t.Fatalf("emails to an address without an account: %d", n)
	}
}

// TestSignupTakesAsLongForAnExistingAddress: signing up with an address
// that has an account hashes the password and sends one email, as a new
// account does, so the time taken doesn't tell which addresses have one.
func TestSignupTakesAsLongForAnExistingAddress(t *testing.T) {
	s, _, box, terms, _ := newAccounts(t)
	ctx := context.Background()
	if _, err := s.SetSignupPolicy(ctx, auth.SignupPolicy{Mode: auth.SignupOpen}); err != nil {
		t.Fatal(err)
	}
	timed := func(addr string) time.Duration {
		start := time.Now()
		if err := s.Signup(ctx, auth.SignupParams{Email: addr, Password: password, TermsVersion: terms}); err != nil {
			t.Fatalf("signup %s: %v", addr, err)
		}
		return time.Since(start)
	}
	fresh, existing := time.Hour, time.Hour
	for i := range 3 {
		fresh = min(fresh, timed("new"+string(rune('a'+i))+"@example.com"))
		existing = min(existing, timed(email))
	}
	if existing < fresh*3/10 {
		t.Fatalf("an existing address answers in %v, a new one in %v", existing, fresh)
	}
	if n := box.count(email); n != 3 {
		t.Fatalf("notices to the existing address: %d", n)
	}
}
