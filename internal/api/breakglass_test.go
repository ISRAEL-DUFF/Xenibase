package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/store"
)

// TestBreakGlassCannotGrantMembership: a platform admin under break-glass
// acts as an org admin, but may not invite (themselves or anyone), change
// roles or transfer ownership, so the access cannot outlive the session;
// nor accept an invitation they sent themselves before this was refused.
// The rest of what break-glass allows still works.
func TestBreakGlassCannotGrantMembership(t *testing.T) {
	ts, clk, db := newAuthServer(t)
	ctx := context.Background()
	admin := newBrowser(t, ts.URL)
	admin.setup(clk)
	var me gen.User
	if code, body := admin.do("GET", "/api/v1/me", nil, &me); code != 200 {
		t.Fatalf("me: %d %s", code, body)
	}

	// Someone else's organisation, with a member and a project.
	owner, member, org := uuid.New(), uuid.New(), uuid.New()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email, password_hash, email_verified_at, approved_at) VALUES ($1, 'ada@example.com', 'x', now(), now()), ($2, 'bob@example.com', 'x', now(), now())`, []any{owner, member}},
		{`INSERT INTO organizations (id, name, slug, plan_id) SELECT $1, 'Acme', 'acme', id FROM quota_plans ORDER BY name LIMIT 1`, []any{org}},
		{`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, []any{org, owner, member}},
	} {
		if _, err := db.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	project := seedProject(t, db, org, "shop")
	if _, err := db.Exec(ctx, `INSERT INTO project_members (project_id, user_id, org_id, role) VALUES ($1, $2, $3, 'developer')`, project, member, org); err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(db).InsertBreakGlass(ctx, store.InsertBreakGlassParams{
		OrgID: org, AdminID: me.Id, Reason: "ticket 42", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/orgs/" + org.String()

	// Still allowed: reading, and what an admin does that hands out nothing.
	if code, body := admin.do("GET", base+"/members", nil, nil); code != 200 {
		t.Fatalf("members under break-glass: %d %s", code, body)
	}
	if code, body := admin.do("PATCH", base, map[string]any{"name": "Acme Ltd"}, nil); code != 200 {
		t.Fatalf("rename under break-glass: %d %s", code, body)
	}

	// Refused: every way of giving someone a role.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", base + "/members", map[string]any{"email": ownerEmail, "role": "admin"}},
		{"POST", base + "/members", map[string]any{"email": "carol@example.com", "role": "member"}},
		{"PATCH", base + "/members/" + member.String(), map[string]any{"role": "admin"}},
		{"POST", "/api/v1/projects/" + project.String() + "/members", map[string]any{"email": ownerEmail, "role": "admin"}},
		{"PATCH", "/api/v1/projects/" + project.String() + "/members/" + member.String(), map[string]any{"role": "admin"}},
	} {
		if code, body := admin.do(c.method, c.path, c.body, nil); code != http.StatusForbidden || !strings.Contains(body, "break_glass") {
			t.Errorf("%s %s under break-glass: %d %s", c.method, c.path, code, body)
		}
	}
	// Transferring ownership needs an owner, which break-glass never is.
	if code, body := admin.do("POST", base+"/transfer-ownership", map[string]any{"user_id": member.String()}, nil); code != http.StatusForbidden {
		t.Errorf("transfer under break-glass: %d %s", code, body)
	}
	var invitations int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM invitations WHERE org_id = $1`, org).Scan(&invitations); err != nil || invitations != 0 {
		t.Fatalf("invitations made under break-glass: %d %v", invitations, err)
	}

	// An invitation to themselves sent before this fix can't be accepted.
	var inv uuid.UUID
	if err := db.QueryRow(ctx, `INSERT INTO invitations (email, token_hash, kind, org_id, org_role, project_roles, invited_by, expires_at)
		VALUES ($1, 'h', 'org', $2, 'admin', '[]', $3, now() + interval '1 day') RETURNING id`, ownerEmail, org, me.Id).Scan(&inv); err != nil {
		t.Fatal(err)
	}
	if code, body := admin.do("POST", "/api/v1/me/invitations/"+inv.String()+"/accept", nil, nil); code != http.StatusForbidden {
		t.Fatalf("accepting one's own invitation: %d %s", code, body)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE org_id = $1 AND user_id = $2`, org, me.Id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the platform admin became a member: %d %v", n, err)
	}
}
