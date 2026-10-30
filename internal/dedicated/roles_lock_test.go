package dedicated_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// rolesEnv is a dedicated project whose "instance" is the test Postgres
// server, reached as its superuser.
type rolesEnv struct {
	*env
	cluster  *pgx.Conn
	project  store.Project
	projects *provision.Service
}

func setupRoles(t *testing.T, withOwner bool) *rolesEnv {
	t.Helper()
	ctx := context.Background()
	db := storetest.New(t)
	cfg, err := pgx.ParseConfig(os.Getenv(storetest.EnvVar))
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cluster.Close(context.Background()) })
	k, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(k)
	ca, err := agentca.LoadOrCreate(ctx, db, kr)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := nodes.NewService(db, ca, "", quiet)
	if err != nil {
		t.Fatal(err)
	}
	pm, err := pooler.NewManager(t.TempDir(), 0o644, db, nil, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ps := provision.NewService(db, kr, pm, provision.Config{AdminSSLMode: "disable"}, quiet)
	e := &env{db: db, svc: dedicated.New(db, kr, ns, ps, secrets{}, dedicated.Config{}, quiet)}

	inst := e.instance(t, e.node(t, "node1", "healthy"), "dedicated", "1 hour")
	sealed, err := provision.SealInstanceSecret(kr, inst.ID, provision.AdminSecret{User: cfg.User, Password: cfg.Password})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE instances SET status = 'running', admin_host = $2, admin_port = $3, admin_secret = $4 WHERE id = $1`,
		inst.ID, cfg.Host, int32(cfg.Port), sealed); err != nil {
		t.Fatal(err)
	}
	dbName, owner, err := provision.Names()
	if err != nil {
		t.Fatal(err)
	}
	var org uuid.UUID
	if err := db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id)
		SELECT 'o', 'o', id FROM quota_plans WHERE name = 'Personal' RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	p, err := store.New(db).InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: org, Name: "p", Slug: "p", DbName: dbName, OwnerRole: owner,
		ScramVerifier: "SCRAM-SHA-256$4096:x$y:z", Tier: provision.TierDedicated, InstanceID: inst.ID,
		Settings: []byte(`{"connection_limit": 20}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE projects SET status = 'active' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	p.Status = provision.StatusActive
	if withOwner {
		if _, err := cluster.Exec(ctx, "CREATE ROLE "+provision.Ident(owner)+" NOCREATEROLE"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = cluster.Exec(context.Background(), "DROP ROLE IF EXISTS "+provision.Ident(owner)) })
	}
	return &rolesEnv{env: e, cluster: cluster, project: p, projects: ps}
}

func (e *rolesEnv) state(t *testing.T) (flag, createRole bool) {
	t.Helper()
	ctx := context.Background()
	p, err := store.New(e.db).GetProject(ctx, e.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.cluster.QueryRow(ctx, `SELECT rolcreaterole FROM pg_roles WHERE rolname = $1`, p.OwnerRole).Scan(&createRole)
	return p.ManageRoles, createRole
}

// Switching role management waits its turn behind an operation that may
// run ensureRole with the old setting, and changes nothing meanwhile.
func TestSetManageRolesRefusesBusyProject(t *testing.T) {
	e := setupRoles(t, true)
	if _, err := jobs.Enqueue(context.Background(), e.db, jobs.EnqueueParams{Kind: provision.KindApplySettings, ProjectID: &e.project.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetManageRoles(context.Background(), e.project, true); !errors.Is(err, provision.ErrConflict) {
		t.Fatalf("switched role management during an operation: %v", err)
	}
	if flag, cr := e.state(t); flag || cr {
		t.Fatalf("flag=%v CREATEROLE=%v after a refused switch", flag, cr)
	}
}

// The instance is changed before the flag: if it refuses, the flag stays.
func TestSetManageRolesKeepsFlagWhenInstanceRefuses(t *testing.T) {
	e := setupRoles(t, false) // ALTER ROLE fails: the owner is missing
	if _, err := e.svc.SetManageRoles(context.Background(), e.project, true); err == nil {
		t.Fatal("switch succeeded without an owner role")
	}
	if flag, _ := e.state(t); flag {
		t.Fatal("the flag says on although the instance refused")
	}
}

func TestSetManageRolesSwitches(t *testing.T) {
	e := setupRoles(t, true)
	if _, err := e.svc.SetManageRoles(context.Background(), e.project, true); err != nil {
		t.Fatal(err)
	}
	if flag, cr := e.state(t); !flag || !cr {
		t.Fatalf("flag=%v CREATEROLE=%v after switching on", flag, cr)
	}
}

// While a project's logins are switched off (SetLogins made its owned
// logins NOLOGIN), the sync keeps them recorded, so switching the logins
// back on (ProjectLogins) finds and restores them. Once the logins are on,
// a NOLOGIN role is the owner's choice and is dropped as before.
func TestSyncOwnedRolesKeepsLoginsSwitchedOff(t *testing.T) {
	e := setupRoles(t, true)
	ctx := context.Background()
	app := e.project.DbName + "_app"
	verifier, err := crypto.SCRAMVerifier("app-password-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE DATABASE " + provision.Ident(e.project.DbName),
		"CREATE ROLE " + provision.Ident(app) + " LOGIN PASSWORD '" + verifier + "'",
		"GRANT " + provision.Ident(app) + " TO " + provision.Ident(e.project.OwnerRole) + " WITH ADMIN OPTION",
	} {
		if _, err := e.cluster.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = e.cluster.Exec(c, "DROP DATABASE IF EXISTS "+provision.Ident(e.project.DbName)+" WITH (FORCE)")
		_, _ = e.cluster.Exec(c, "DROP ROLE IF EXISTS "+provision.Ident(app))
	})
	synced := func() []string {
		t.Helper()
		res, err := e.svc.SyncOwnedRoles(ctx, e.project)
		if err != nil {
			t.Fatal(err)
		}
		return res.Synced
	}
	if got := synced(); len(got) != 1 || got[0] != app {
		t.Fatalf("synced: %v", got)
	}
	// Suspended: SetLogins switched it off.
	on := false
	e.projects.LoginGate = func(context.Context, store.Project) (bool, error) { return on, nil }
	if _, err := e.cluster.Exec(ctx, "ALTER ROLE "+provision.Ident(app)+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	if got := synced(); len(got) != 1 || got[0] != app {
		t.Fatalf("a login switched off by a suspension was dropped from the record: %v", got)
	}
	roles, err := e.projects.ProjectLogins(ctx, e.project)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(roles, app) {
		t.Fatalf("ProjectLogins after the sync: %v", roles)
	}
	on = true
	if got := synced(); len(got) != 0 {
		t.Fatalf("a NOLOGIN role with the logins on is still synced: %v", got)
	}
}
