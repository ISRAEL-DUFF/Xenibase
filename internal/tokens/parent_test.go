package tokens_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/store/storetest"
	"github.com/israel-duff/xenibase/internal/tokens"
)

// TestRevokingATokenRevokesWhatItMade: a token created by another token
// goes when its parent goes, however the parent is revoked (by its user,
// with its OAuth app, or by the app itself), and so do its own children.
func TestRevokingATokenRevokesWhatItMade(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	key, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(key)
	svc := tokens.New(db, kr, nil, tokens.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	user, org, admin := uuid.New(), uuid.New(), uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email, password_hash, email_verified_at, approved_at) VALUES ($1, 'ada@example.com', 'x', now(), now()), ($2, 'root@example.com', 'x', now(), now())`, []any{user, admin}},
		{`INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Org', 'org', id FROM quota_plans ORDER BY name LIMIT 1`, []any{org}},
	} {
		if _, err := db.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	family := func(top tokens.CreateParams) (parent, child, grandchild tokens.Created) {
		t.Helper()
		var err error
		if parent, err = svc.Create(ctx, top); err != nil {
			t.Fatal(err)
		}
		mk := func(of tokens.Created, name string) tokens.Created {
			c, err := svc.Create(ctx, tokens.CreateParams{UserID: user, OrgID: org, Name: name, Scopes: []string{"read"}, Via: "api", ParentID: &of.Row.ID})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		child = mk(parent, "child")
		grandchild = mk(child, "grandchild")
		return parent, child, grandchild
	}
	works := func(c tokens.Created) bool {
		_, err := svc.Authenticate(ctx, c.Secret, nil)
		if err != nil && !errors.Is(err, tokens.ErrUnauthenticated) {
			t.Fatal(err)
		}
		return err == nil
	}

	// Revoked by its user.
	p, c, g := family(tokens.CreateParams{UserID: user, OrgID: org, Name: "laptop", Scopes: []string{"read", "write"}})
	if !works(c) || !works(g) {
		t.Fatal("children don't work to begin with")
	}
	if err := svc.RevokeOwn(ctx, user, p.Row.ID); err != nil {
		t.Fatal(err)
	}
	if works(p) || works(c) || works(g) {
		t.Fatalf("after revoking the parent: %v %v %v", works(p), works(c), works(g))
	}

	// Revoking a child leaves its parent.
	p, c, g = family(tokens.CreateParams{UserID: user, OrgID: org, Name: "ci", Scopes: []string{"read", "write"}})
	if err := svc.RevokeInOrg(ctx, org, c.Row.ID, admin); err != nil {
		t.Fatal(err)
	}
	if !works(p) || works(c) || works(g) {
		t.Fatalf("after revoking the child: %v %v %v", works(p), works(c), works(g))
	}

	// Disabling the OAuth app that holds the parent.
	app, err := svc.CreateClient(ctx, tokens.ClientParams{Name: "Taskiem", RedirectURIs: []string{"https://app.taskiem.test/cb"}})
	if err != nil {
		t.Fatal(err)
	}
	p, c, g = family(tokens.CreateParams{UserID: user, OrgID: org, Name: "Taskiem", Scopes: []string{"read", "write"}, Via: "oauth", OAuthClientID: &app.Row.ID})
	if err := svc.DisableClient(ctx, app.Row.ClientID, admin); err != nil {
		t.Fatal(err)
	}
	if works(p) || works(c) || works(g) {
		t.Fatalf("after disabling the app: %v %v %v", works(p), works(c), works(g))
	}
}
