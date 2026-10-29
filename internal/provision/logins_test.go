package provision

import (
	"context"
	"testing"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/store"
)

// TestSetLoginsCoversOwnedLogins: switching a project's logins off (a
// suspension, a hard lock) also switches off the logins its owner created
// with role management (project_owned_logins), and ends their sessions;
// switching them on restores them. Before, ProjectLogins left them out,
// so they could still log in at the instance while suspended.
func TestSetLoginsCoversOwnedLogins(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, _ := h.create(t)
	app := p.DbName + "_app"
	verifier, err := crypto.SCRAMVerifier("app-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.cluster.Exec(ctx, "CREATE ROLE "+ident(app)+" LOGIN PASSWORD "+literal(verifier)); err != nil {
		t.Fatal(err)
	}
	if err := store.New(h.db).InsertOwnedLogin(ctx, store.InsertOwnedLoginParams{ProjectID: p.ID, RoleName: app, ScramVerifier: verifier}); err != nil {
		t.Fatal(err)
	}
	roles, err := h.s.ProjectLogins(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, r := range roles {
		listed = listed || r == app
	}
	if !listed {
		t.Fatalf("ProjectLogins leaves out the owned login: %v", roles)
	}
	canLogin := func() bool {
		t.Helper()
		var ok bool
		if err := h.cluster.QueryRow(ctx, `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, app).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if err := h.s.SetLogins(ctx, p, false); err != nil {
		t.Fatal(err)
	}
	if canLogin() {
		t.Fatal("the owned login can still log in with the project's logins off")
	}
	if err := h.s.SetLogins(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if !canLogin() {
		t.Fatal("the owned login stayed off after the logins were switched back on")
	}
}
