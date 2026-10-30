package provision

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// harness is a Service against a throwaway metadata database, with the
// test Postgres server (XENIBASE_TEST_DATABASE_URL, a superuser) as its one
// shared cluster and poolers that are only configuration files. It covers
// the flows' steps against a real cluster; the smoke test, which needs
// PgBouncer, is the integration suite's.
type harness struct {
	s       *Service
	db      *pgxpool.Pool
	cluster *pgx.Conn // superuser on the shared cluster
	inst    store.Instance
	org     uuid.UUID
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db := storetest.New(t)
	adminURL := os.Getenv(storetest.EnvVar)
	cfg, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cluster.Close(context.Background()) })
	var major int
	if err := cluster.QueryRow(ctx, `SELECT current_setting('server_version_num')::int / 10000`).Scan(&major); err != nil {
		t.Fatal(err)
	}

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := crypto.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	q := store.New(db)
	if err := q.EnsureRegion(ctx, store.EnsureRegionParams{ID: "eu-central", Name: "eu-central"}); err != nil {
		t.Fatal(err)
	}
	// As RegisterSharedCluster, without hardening the test server.
	node, err := q.UpsertNode(ctx, store.UpsertNodeParams{Name: "test", PrivateAddr: cfg.Host, Role: "both"})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealAdminSecret(keyring, node.ID, AdminSecret{User: cfg.User, Password: cfg.Password})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetNodeAdminSecret(ctx, store.SetNodeAdminSecretParams{ID: node.ID, PgAdminSecret: sealed}); err != nil {
		t.Fatal(err)
	}
	host, port := cfg.Host, int32(cfg.Port)
	inst, err := q.UpsertSharedInstance(ctx, store.UpsertSharedInstanceParams{
		NodeID: node.ID, PgVersion: int32(major), Port: port, AdminHost: &host, AdminPort: &port,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE nodes SET status = 'healthy' WHERE id = $1`, node.ID); err != nil {
		t.Fatal(err)
	}
	var org uuid.UUID
	if err := db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id, settings)
		SELECT 'Test', 'test-' || substr(md5(random()::text), 1, 8), id, '{}' FROM quota_plans ORDER BY created_at LIMIT 1
		RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}

	pm, err := pooler.NewManager(t.TempDir(), 0o644, db, nil, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	pm.SetHome("eu-central")
	s := NewService(db, keyring, pm, Config{
		HomeRegion: "eu-central", DBHost: "localhost", SessionPort: 5432, PooledPort: 6543,
		AdminSSLMode: "disable", PGVersions: []int{major},
	}, quiet)
	return &harness{s: s, db: db, cluster: cluster, inst: inst, org: org}
}

// create records a new shared project and runs its create flow up to the
// smoke test: role, database and hardening on the cluster.
func (h *harness) create(t *testing.T) (store.Project, store.Operation) {
	t.Helper()
	ctx := context.Background()
	c, err := h.s.Create(ctx, CreateParams{OrgID: h.org, Name: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	p := c.Project
	t.Cleanup(func() { h.dropObjects(p.DbName) })
	set, err := store.DecodeProjectSettings(p.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.s.ensureRole(ctx, p, set, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.s.ensureDatabase(ctx, p, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.s.hardenDatabase(ctx, p, nil); err != nil {
		t.Fatal(err)
	}
	return p, c.Operation
}

// claim takes op as a worker would, so its step log can be written.
func (h *harness) claim(t *testing.T, op store.Operation) (store.Operation, *jobs.StepLogger) {
	t.Helper()
	got, err := store.New(h.db).ClaimOperation(context.Background(), store.ClaimOperationParams{Worker: "test", Kinds: []string{op.Kind}})
	if err != nil {
		t.Fatal(err)
	}
	return got, jobs.NewStepLogger(h.db, got, "test", quiet)
}

func (h *harness) databaseExists(t *testing.T, db string) bool {
	t.Helper()
	var ok bool
	if err := h.cluster.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, db).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func (h *harness) roleExists(t *testing.T, role string) bool {
	t.Helper()
	var ok bool
	if err := h.cluster.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, role).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// dropObjects removes what a test left on the shared cluster.
func (h *harness) dropObjects(db string) {
	ctx := context.Background()
	_, _ = h.cluster.Exec(ctx, "DROP DATABASE IF EXISTS "+ident(db)+" WITH (FORCE)")
	rows, err := h.cluster.Query(ctx, `SELECT rolname FROM pg_roles WHERE starts_with(rolname, $1)`, db+"_")
	if err != nil {
		return
	}
	roles, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	for _, r := range roles {
		_, _ = h.cluster.Exec(ctx, "DROP OWNED BY "+ident(r))
		_, _ = h.cluster.Exec(ctx, "DROP ROLE IF EXISTS "+ident(r))
	}
}
