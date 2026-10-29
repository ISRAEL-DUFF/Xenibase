package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestHookSecretOnlyForProjectAdmins: the auth settings show the hook
// signing secret to a project admin; a developer, who may read the
// settings, sees only that there is one.
func TestHookSecretOnlyForProjectAdmins(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	org, project, admin, dev := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		for _, st := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO organizations (id, name, slug, plan_id, status) SELECT $1, 'acme', 'acme', id, 'active' FROM quota_plans LIMIT 1`, []any{org}},
			{`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member'), ($1, $3, 'member')`, []any{org, admin, dev}},
			{`INSERT INTO project_members (project_id, user_id, org_id, role) VALUES ($1, $2, $4, 'admin'), ($1, $3, $4, 'developer')`,
				[]any{project, admin, dev, org}},
		} {
			if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	as := func(user uuid.UUID) context.Context {
		return context.WithValue(ctx, keyAccess, access{Actor: authz.Actor{Kind: authz.ActorSession, UserID: user},
			OrgID: org, ProjectID: project})
	}
	for _, c := range []struct {
		who    string
		user   uuid.UUID
		secret string
		shown  bool
	}{
		{"an admin", admin, "whsec_abc", true},
		{"a developer", dev, "whsec_abc", false},
		{"an admin, no secret", admin, "", false},
	} {
		var out gen.AuthConfig
		if err := s.hookSecretOut(as(c.user), &out, c.secret); err != nil {
			t.Fatal(err)
		}
		if (out.HookSecret != nil) != c.shown || (c.shown && *out.HookSecret != c.secret) {
			t.Errorf("%s: hook_secret %v, want shown=%v", c.who, out.HookSecret, c.shown)
		}
		if out.HookSecretSet == nil || *out.HookSecretSet != (c.secret != "") {
			t.Errorf("%s: hook_secret_set %v", c.who, out.HookSecretSet)
		}
	}
}
