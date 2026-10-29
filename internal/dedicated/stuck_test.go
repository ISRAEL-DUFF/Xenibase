package dedicated_test

import (
	"context"
	"encoding/json"
	"os"
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

// moveEnv is a project frozen part-way through a move: its instance is the
// test Postgres server, its owner NOLOGIN there, and the poolers one fake
// PgBouncer console.
type moveEnv struct {
	*env
	cluster *pgx.Conn
	bouncer *fakeBouncer
	source  store.Instance
	target  store.Instance
	project store.Project
	port    int32
}

func setupMove(t *testing.T, status string) *moveEnv {
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
	fb := startFakeBouncer(t)
	pm, err := pooler.NewManager(t.TempDir(), 0o644, db, []*pooler.Admin{fb.admin(t)}, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ps := provision.NewService(db, kr, pm, provision.Config{AdminSSLMode: "disable"}, quiet)
	e := &env{db: db, svc: dedicated.New(db, kr, ns, ps, secrets{}, dedicated.Config{}, quiet)}

	n := e.node(t, "node1", "healthy")
	src := e.instance(t, n, "dedicated", "1 day")
	sealed, err := provision.SealInstanceSecret(kr, src.ID, provision.AdminSecret{User: cfg.User, Password: cfg.Password})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE instances SET status = 'running', admin_host = $2, admin_port = $3, admin_secret = $4 WHERE id = $1`,
		src.ID, cfg.Host, int32(cfg.Port), sealed); err != nil {
		t.Fatal(err)
	}
	dst := e.instance(t, n, "dedicated", "10 minutes")
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
		ScramVerifier: "SCRAM-SHA-256$4096:x$y:z", Tier: provision.TierDedicated, InstanceID: src.ID, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.New(db).SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: status}); err != nil {
		t.Fatal(err)
	}
	p.Status = status
	// As freeze leaves it.
	if _, err := cluster.Exec(ctx, "CREATE ROLE "+provision.Ident(owner)+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = cluster.Exec(context.Background(), "DROP ROLE IF EXISTS "+provision.Ident(owner)) })
	return &moveEnv{env: e, cluster: cluster, bouncer: fb, source: src, target: dst, project: p, port: int32(cfg.Port)}
}

// reachable points the source at the test server, or at a closed port.
func (e *moveEnv) reachable(t *testing.T, ok bool) {
	t.Helper()
	port := int32(1)
	if ok {
		port = e.port
	}
	if _, err := e.db.Exec(context.Background(), `UPDATE instances SET admin_port = $2 WHERE id = $1`, e.source.ID, port); err != nil {
		t.Fatal(err)
	}
}

// op queues and claims an operation of kind on the project, moving it from
// the source to the target, on its attempt'th attempt.
func (e *moveEnv) op(t *testing.T, kind string, attempt int, newInstance bool) (store.Operation, *jobs.StepLogger) {
	t.Helper()
	ctx := context.Background()
	params, _ := json.Marshal(map[string]any{"target_instance": e.target.ID, "source_instance": e.source.ID, "new_instance": newInstance})
	if _, err := jobs.Enqueue(ctx, e.db, jobs.EnqueueParams{Kind: kind, ProjectID: &e.project.ID, Params: json.RawMessage(params)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(ctx, `UPDATE operations SET attempts = $2 - 1 WHERE project_id = $1`, e.project.ID, attempt); err != nil {
		t.Fatal(err)
	}
	got, err := store.New(e.db).ClaimOperation(ctx, store.ClaimOperationParams{Worker: "test", Kinds: []string{kind}})
	if err != nil {
		t.Fatal(err)
	}
	return got, jobs.NewStepLogger(e.db, got, "test", quiet)
}

func (e *moveEnv) canLogin(t *testing.T) bool {
	t.Helper()
	var ok bool
	if err := e.cluster.QueryRow(context.Background(), `SELECT rolcanlogin FROM pg_roles WHERE rolname = $1`, e.project.OwnerRole).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func (e *moveEnv) status(t *testing.T) string {
	t.Helper()
	p, err := store.New(e.db).GetProject(context.Background(), e.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p.Status
}

// A rollback that cannot reach the source used to return before resuming
// the poolers and leave the project moving, paused and NOLOGIN for good.
// Now the poolers resume at once, and the sweep finishes the rollback once
// the source answers.
func TestFailedRollbackResumesAndIsRepaired(t *testing.T) {
	for _, tc := range []struct {
		kind, status string
		fail         func(*dedicated.Service, context.Context, store.Operation, *jobs.StepLogger, error) error
	}{
		{dedicated.KindMove, provision.StatusMoving, dedicated.FailMove},
		{dedicated.KindUpgrade, provision.StatusUpgrading, dedicated.FailMove},
		{dedicated.KindPromote, provision.StatusPromoting, dedicated.FailPromote},
		{dedicated.KindDemote, provision.StatusDemoting, dedicated.FailDemote},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			e := setupMove(t, tc.status)
			ctx := context.Background()
			resume := "RESUME " + e.project.DbName
			e.reachable(t, false)
			op, log := e.op(t, tc.kind, 2, false)

			if err := tc.fail(e.svc, ctx, op, log, nil); err == nil {
				t.Fatal("the rollback reported success though the source's logins are still NOLOGIN")
			}
			if e.bouncer.count(resume) != 1 {
				t.Fatal("the rollback did not resume the poolers because the source was unreachable")
			}
			if st := e.status(t); st != tc.status {
				t.Fatalf("project is %s after an incomplete rollback, want it kept %s for the sweep", st, tc.status)
			}

			// Nothing to repair while the operation still runs.
			if n, err := e.svc.RepairStuckMoves(ctx); err != nil || n != 0 {
				t.Fatalf("RepairStuckMoves during the operation = %d, %v", n, err)
			}
			if _, err := e.db.Exec(ctx, `UPDATE operations SET status = 'failed', finished_at = now() - interval '2 minutes' WHERE id = $1`, op.ID); err != nil {
				t.Fatal(err)
			}
			// The source still down: it stays for the next pass.
			if n, err := e.svc.RepairStuckMoves(ctx); err == nil || n != 0 {
				t.Fatalf("RepairStuckMoves with the source down = %d, %v; want an error", n, err)
			}
			if st := e.status(t); st != tc.status {
				t.Fatalf("project is %s after a failed repair", st)
			}

			e.reachable(t, true)
			if n, err := e.svc.RepairStuckMoves(ctx); err != nil || n != 1 {
				t.Fatalf("RepairStuckMoves = %d, %v; want 1", n, err)
			}
			if !e.canLogin(t) {
				t.Fatal("the owner is still NOLOGIN after the repair")
			}
			if st := e.status(t); st != provision.StatusActive {
				t.Fatalf("project is %s after the repair, want active", st)
			}
			if e.bouncer.count(resume) < 2 {
				t.Fatal("the repair did not resume the poolers")
			}
		})
	}
}
