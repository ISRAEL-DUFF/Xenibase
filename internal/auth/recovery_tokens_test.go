package auth_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/auth"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestRecoveryRevokesTokens: taking an account back (a password reset, a
// two-factor reset, or a password change that asks to) revokes its API
// tokens, which a stolen session could have minted; the emails say so.
func TestRecoveryRevokesTokens(t *testing.T) {
	ctx := context.Background()
	pool := storetest.New(t)
	key, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(key)
	c := &clock{t: time.Now()}
	s := auth.NewService(pool, kr, auth.Config{Now: c.Now, PublicURL: "https://xenibase.test"}, "setup-code", slog.New(slog.NewTextHandler(io.Discard, nil)))
	box := &inbox{}
	s.SetMailer(box)
	secret := setupOwner(t, s, c)
	u, err := store.New(pool).GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	org := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Org', 'org', id FROM quota_plans ORDER BY name LIMIT 1`, org); err != nil {
		t.Fatal(err)
	}
	mint := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO api_tokens (user_id, org_id, name, token_hash, prefix, scopes, expires_at)
			VALUES ($1, $2, 'ci', $3, 'xb_abcdef', '{read,write}', now() + interval '90 days')`, u.ID, org, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	live := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE user_id = $1 AND revoked_at IS NULL`, u.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A password change keeps them when asked to, and revokes them otherwise.
	mint()
	_, sess := login(t, s, c, secret)
	if err := s.ChangePassword(ctx, sess, password, "a different long password", false); err != nil {
		t.Fatal(err)
	}
	if live() != 1 {
		t.Fatal("a password change that keeps tokens revoked them")
	}
	if err := s.ChangePassword(ctx, sess, "a different long password", password, true); err != nil {
		t.Fatal(err)
	}
	if live() != 0 {
		t.Fatal("a password change that revokes tokens kept them")
	}

	// A reset by email always revokes them, and its email says it will.
	mint()
	if err := s.RequestPasswordReset(ctx, email); err != nil {
		t.Fatal(err)
	}
	last := box.msgs[len(box.msgs)-1]
	if !strings.Contains(last.Body, "revokes your API tokens") {
		t.Fatalf("the reset email doesn't say tokens go: %s", last.Body)
	}
	if err := s.ResetPassword(ctx, box.lastToken(t, email), "yet another long password"); err != nil {
		t.Fatal(err)
	}
	if live() != 0 {
		t.Fatal("a password reset kept the account's tokens")
	}

	// So does a two-factor reset.
	mint()
	if err := s.ResetTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if live() != 0 {
		t.Fatal("a two-factor reset kept the account's tokens")
	}
	if last := box.msgs[len(box.msgs)-1]; !strings.Contains(last.Body, "API tokens were revoked") {
		t.Fatalf("the two-factor reset email doesn't say tokens went: %s", last.Body)
	}
}
