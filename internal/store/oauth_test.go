package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

func TestOAuthClientsAndCodes(t *testing.T) {
	ctx := context.Background()
	pool := storetest.New(t)
	q := store.New(pool)
	u, err := q.InsertUser(ctx, store.InsertUserParams{Email: "t@example.com", PasswordHash: "x", PlatformRole: "user"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := q.GetPlanByName(ctx, "Personal")
	if err != nil {
		t.Fatal(err)
	}
	o, err := q.InsertOrg(ctx, store.InsertOrgParams{Name: "T", Slug: "t", PersonalOwnerID: &u.ID, PlanID: plan.ID, Settings: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}

	hash := "deadbeef"
	c, err := q.InsertOAuthClient(ctx, store.InsertOAuthClientParams{
		ClientID: "xbapp_1", Name: "Taskiem", ClientSecretHash: &hash, RedirectUris: []string{"https://a.test/cb"}, ScopesAllowed: []string{"read", "write"}, CreatedBy: &u.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertOAuthClient(ctx, store.InsertOAuthClientParams{ClientID: "xbapp_1", Name: "Dup", RedirectUris: []string{"https://a.test/cb"}, ScopesAllowed: []string{"read"}}); err == nil {
		t.Fatal("client_id should be unique")
	}
	if _, err := q.InsertOAuthClient(ctx, store.InsertOAuthClientParams{ClientID: "xbapp_2", Name: "Bad", RedirectUris: []string{"https://a.test/cb"}, ScopesAllowed: []string{"root"}}); err == nil {
		t.Fatal("scopes_allowed should be checked")
	}
	got, err := q.GetOAuthClient(ctx, "xbapp_1")
	if err != nil || got.ID != c.ID || got.ClientSecretHash == nil || *got.ClientSecretHash != hash || got.DisabledAt != nil {
		t.Fatalf("get client: %+v %v", got, err)
	}

	// A code, used once; the token it issued is remembered.
	exp := time.Now().Add(10 * time.Minute)
	days := int32(30)
	if err := q.InsertOAuthCode(ctx, store.InsertOAuthCodeParams{
		CodeHash: "h1", ClientID: "xbapp_1", UserID: u.ID, OrgID: o.ID, ProjectIds: nil, Scopes: []string{"read"},
		ExpiresInDays: &days, RedirectUri: "https://a.test/cb", CodeChallenge: "ch", ExpiresAt: exp,
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.InsertOAuthCode(ctx, store.InsertOAuthCodeParams{CodeHash: "h2", ClientID: "xbapp_1", UserID: u.ID, OrgID: o.ID, Scopes: []string{"root"}, RedirectUri: "x", CodeChallenge: "c", ExpiresAt: exp}); err == nil {
		t.Fatal("code scopes should be checked")
	}
	tok, err := q.InsertAPIToken(ctx, store.InsertAPITokenParams{
		UserID: u.ID, OrgID: o.ID, Name: "Taskiem", TokenHash: "th", Prefix: "xb_abc", Scopes: []string{"read"}, ExpiresAt: exp, CreatedVia: "oauth", OauthClientID: &c.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertAPIToken(ctx, store.InsertAPITokenParams{UserID: u.ID, OrgID: o.ID, Name: "x", TokenHash: "th2", Prefix: "xb_", Scopes: []string{"read"}, ExpiresAt: exp, CreatedVia: "magic"}); err == nil {
		t.Fatal("created_via should be checked")
	}
	code, err := q.GetOAuthCodeForUpdate(ctx, "h1")
	if err != nil || code.UsedAt != nil || code.ExpiresInDays == nil || *code.ExpiresInDays != 30 || code.ProjectIds != nil {
		t.Fatalf("code: %+v %v", code, err)
	}
	if n, err := q.UseOAuthCode(ctx, store.UseOAuthCodeParams{CodeHash: "h1", OrgID: o.ID, TokenID: &tok.ID}); err != nil || n != 1 {
		t.Fatalf("use: %d %v", n, err)
	}
	if n, _ := q.UseOAuthCode(ctx, store.UseOAuthCodeParams{CodeHash: "h1", OrgID: o.ID, TokenID: &tok.ID}); n != 0 {
		t.Fatal("a used code was used again")
	}
	code, _ = q.GetOAuthCodeForUpdate(ctx, "h1")
	if code.UsedAt == nil || code.TokenID == nil || *code.TokenID != tok.ID {
		t.Fatalf("used code: %+v", code)
	}
	if _, err := q.GetOAuthCodeForUpdate(ctx, "nope"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unknown code: %v", err)
	}

	// Listings carry the app's name; the app finds its own token by hash.
	mine, err := q.ListUserTokens(ctx, u.ID)
	if err != nil || len(mine) != 1 || mine[0].OauthClientName == nil || *mine[0].OauthClientName != "Taskiem" {
		t.Fatalf("user tokens: %+v %v", mine, err)
	}
	orgTokens, err := q.ListOrgTokens(ctx, o.ID)
	if err != nil || len(orgTokens) != 1 || orgTokens[0].OauthClientName == nil || *orgTokens[0].OauthClientName != "Taskiem" {
		t.Fatalf("org tokens: %+v %v", orgTokens, err)
	}
	if got, err := q.GetOAuthClientToken(ctx, store.GetOAuthClientTokenParams{TokenHash: "th", OauthClientID: &c.ID}); err != nil || got.ID != tok.ID {
		t.Fatalf("client token: %v", err)
	}
	otherClient := uuid.New()
	if _, err := q.GetOAuthClientToken(ctx, store.GetOAuthClientTokenParams{TokenHash: "th", OauthClientID: &otherClient}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another client's token: %v", err)
	}

	// Disabling an app revokes its tokens; a second disable changes nothing.
	if n, err := q.DisableOAuthClient(ctx, "xbapp_1"); err != nil || n != 1 {
		t.Fatalf("disable: %d %v", n, err)
	}
	if n, _ := q.DisableOAuthClient(ctx, "xbapp_1"); n != 0 {
		t.Fatal("disabled twice")
	}
	if n, err := q.RevokeOAuthClientTokens(ctx, store.RevokeOAuthClientTokensParams{OauthClientID: &c.ID}); err != nil || n != 1 {
		t.Fatalf("revoke client tokens: %d %v", n, err)
	}
	all, _ := q.ListOAuthClients(ctx)
	if len(all) != 1 || all[0].DisabledAt == nil {
		t.Fatalf("list: %+v", all)
	}

	// The sweep keeps fresh codes and drops stale ones.
	if _, err := pool.Exec(ctx, `UPDATE oauth_codes SET expires_at = now() - interval '2 hours' WHERE code_hash = 'h1'`); err != nil {
		t.Fatal(err)
	}
	if n, err := q.DeleteExpiredOAuthCodes(ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
}

func TestProjectIntegratorRoles(t *testing.T) {
	ctx := context.Background()
	pool := storetest.New(t)
	q := store.New(pool)
	u, err := q.InsertUser(ctx, store.InsertUserParams{Email: "t@example.com", PasswordHash: "x", PlatformRole: "user"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := q.GetPlanByName(ctx, "Personal")
	if err != nil {
		t.Fatal(err)
	}
	o, err := q.InsertOrg(ctx, store.InsertOrgParams{Name: "T", Slug: "t", PersonalOwnerID: &u.ID, PlanID: plan.ID, Settings: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	node, inst := uuid.New(), uuid.New()
	for _, stmt := range []string{
		`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ('` + node.String() + `', 'n1', '10.0.0.1', 'both', 'fp', '{}')`,
		`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ('` + inst.String() + `', '` + node.String() + `', 'dedicated', 18, 5432, 'running')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	p, err := q.InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: o.ID, Name: "P", Slug: "p", DbName: "p_abcd", OwnerRole: "p_abcd_owner",
		ScramVerifier: "x", Tier: "dedicated", InstanceID: inst, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.IntegratorRoles != nil {
		t.Fatalf("new project has integrator roles: %v", p.IntegratorRoles)
	}
	if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	roles := []string{"taskiem_app", "taskiem_dispatch"}
	if err := q.SetProjectIntegratorRoles(ctx, store.SetProjectIntegratorRolesParams{ID: p.ID, IntegratorRoles: roles}); err != nil {
		t.Fatal(err)
	}
	got, err := q.GetProject(ctx, p.ID)
	if err != nil || !slices.Equal(got.IntegratorRoles, roles) {
		t.Fatalf("roles: %v %v", got.IntegratorRoles, err)
	}
	// The isolation audit sees them with the project.
	ips, err := q.InstanceProjects(ctx, inst)
	if err != nil || len(ips) != 1 || !slices.Equal(ips[0].IntegratorRoles, roles) {
		t.Fatalf("instance projects: %+v %v", ips, err)
	}
	if err := q.SetProjectIntegratorRoles(ctx, store.SetProjectIntegratorRolesParams{ID: p.ID, IntegratorRoles: nil}); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.GetProject(ctx, p.ID); got.IntegratorRoles != nil {
		t.Fatalf("cleared roles: %v", got.IntegratorRoles)
	}
}
