package freetier

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/backup"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// harness is a Free tier Service against a throwaway metadata database,
// with the test Postgres server (XENIBASE_TEST_DATABASE_URL, a superuser)
// as its one shared cluster, poolers that are only configuration files,
// and the real backup service, which has no node agents: dumps and
// restores fail, and fakeArchiver stands in where a test needs them.
type harness struct {
	s       *Service
	db      *pgxpool.Pool
	cluster *pgx.Conn
	inst    store.Instance
	org     uuid.UUID
	now     time.Time
}

func newHarness(t *testing.T) *harness {
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
	node, err := q.UpsertNode(ctx, store.UpsertNodeParams{Name: "test", PrivateAddr: cfg.Host, Role: "both"})
	if err != nil {
		t.Fatal(err)
	}
	host, port := cfg.Host, int32(cfg.Port)
	inst, err := q.UpsertSharedInstance(ctx, store.UpsertSharedInstanceParams{
		NodeID: node.ID, PgVersion: int32(major), Port: port, AdminHost: &host, AdminPort: &port,
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := provision.SealInstanceSecret(keyring, inst.ID, provision.AdminSecret{User: cfg.User, Password: cfg.Password})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE instances SET admin_secret = $2 WHERE id = $1`, inst.ID, sealed); err != nil {
		t.Fatal(err)
	}
	var org uuid.UUID
	if err := db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id, settings)
		SELECT 'Test', 'test-' || substr(md5(random()::text), 1, 8), id, '{}' FROM quota_plans WHERE name = 'Personal'
		RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	pm, err := pooler.NewManager(t.TempDir(), 0o644, db, nil, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	pm.SetHome("eu-central")
	ps := provision.NewService(db, keyring, pm, provision.Config{
		HomeRegion: "eu-central", DBHost: "localhost", SessionPort: 5432, PooledPort: 6543,
		AdminSSLMode: "disable", PGVersions: []int{major},
	}, quiet)
	ca, err := agentca.LoadOrCreate(ctx, db, keyring)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := nodes.NewService(db, ca, "", quiet)
	if err != nil {
		t.Fatal(err)
	}
	bs := backup.NewService(db, keyring, ns, ps, backup.Config{}, quiet)
	h := &harness{db: db, cluster: cluster, inst: inst, org: org, now: time.Now()}
	h.s = New(db, ps, bs, nil, Config{Now: func() time.Time { return h.now }}, quiet)
	return h
}

// project records an active Free project with a real database (and owner
// role) on the cluster, in lifecycle, idle since long ago.
func (h *harness) project(t *testing.T, lifecycle string) store.Project {
	t.Helper()
	ctx := context.Background()
	q := store.New(h.db)
	name := "p_" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
	p, err := q.InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: h.org, Name: name, Slug: strings.ReplaceAll(name, "_", "-"), DbName: name, OwnerRole: name + "_owner",
		ScramVerifier: "x", Tier: provision.TierShared, InstanceID: h.inst.ID, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: provision.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(ctx, `UPDATE projects SET lifecycle = $2, last_active_at = now() - interval '200 days',
		paused_at = CASE WHEN $2 = 'paused' THEN now() - interval '100 days' END WHERE id = $1`, p.ID, lifecycle); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE ROLE " + provision.Ident(p.OwnerRole),
		"CREATE DATABASE " + provision.Ident(p.DbName) + " OWNER " + provision.Ident(p.OwnerRole),
	} {
		if _, err := h.cluster.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = h.cluster.Exec(ctx, "DROP DATABASE IF EXISTS "+provision.Ident(p.DbName)+" WITH (FORCE)")
		_, _ = h.cluster.Exec(ctx, "DROP ROLE IF EXISTS "+provision.Ident(p.OwnerRole))
	})
	return h.get(t, p.ID)
}

func (h *harness) get(t *testing.T, id uuid.UUID) store.Project {
	t.Helper()
	p, err := store.New(h.db).GetProject(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// hasDatabase reports whether p's database is still on the cluster.
func (h *harness) hasDatabase(t *testing.T, p store.Project) bool {
	t.Helper()
	var ok bool
	if err := h.cluster.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, p.DbName).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// setPlan puts the harness's organisation on billing plan (free, pro,
// team) with organisation status.
func (h *harness) setPlan(t *testing.T, plan, status string) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO price_books (version, effective_at, prices) VALUES (1, now(), '{}') ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO billing_accounts (org_id, plan, price_book_version) VALUES ($1, $2, 1) ON CONFLICT (org_id) DO UPDATE SET plan = $2`, []any{h.org, plan}},
		{`UPDATE organizations SET status = $2 WHERE id = $1`, []any{h.org, status}},
	} {
		if _, err := h.db.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}
}

// accepts reports whether p's database accepts connections.
func (h *harness) accepts(t *testing.T, p store.Project) bool {
	t.Helper()
	var ok bool
	if err := h.cluster.QueryRow(context.Background(), `SELECT datallowconn FROM pg_database WHERE datname = $1`, p.DbName).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// operation queues kind on p and claims it as a worker would.
func (h *harness) operation(t *testing.T, p store.Project, kind string) (store.Operation, *jobs.StepLogger) {
	t.Helper()
	ctx := context.Background()
	if _, err := jobs.Enqueue(ctx, h.db, jobs.EnqueueParams{Kind: kind, ProjectID: &p.ID}); err != nil {
		t.Fatal(err)
	}
	op, err := store.New(h.db).ClaimOperation(ctx, store.ClaimOperationParams{Worker: "test", Kinds: []string{kind}})
	if err != nil {
		t.Fatal(err)
	}
	return op, jobs.NewStepLogger(h.db, op, "test", quiet)
}

// archiveBackup records a finished archive backup of p taken by op,
// verified or not, as an earlier attempt would have left it.
func (h *harness) archiveBackup(t *testing.T, p store.Project, op store.Operation, verified bool) store.Backup {
	t.Helper()
	ctx := context.Background()
	q := store.New(h.db)
	b, err := q.InsertBackup(ctx, store.InsertBackupParams{ProjectID: &p.ID, Kind: backup.Archive, ObjectKey: "archive/" + p.ID.String() + ".dump.enc",
		KeyWrapped: []byte{0}, OperationID: &op.ID})
	if err != nil {
		t.Fatal(err)
	}
	size, sum := int64(4), "x"
	if b, err = q.FinishBackup(ctx, store.FinishBackupParams{ID: b.ID, SizeBytes: &size, Checksum: &sum}); err != nil {
		t.Fatal(err)
	}
	if verified {
		if b, err = q.MarkBackupVerified(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// fakeArchiver stands in for the backup service: its verification passes
// or fails as told, recording the result on the row as the real one does.
type fakeArchiver struct {
	db       *pgxpool.Pool
	fail     error
	archived int
	verified []uuid.UUID
	// take, when set, takes a new backup; otherwise Archive fails.
	take func(ctx context.Context, p store.Project, opID *uuid.UUID) (store.Backup, error)
}

func (f *fakeArchiver) Archive(ctx context.Context, p store.Project, opID *uuid.UUID, _ *jobs.StepLogger) (store.Backup, error) {
	f.archived++
	if f.take != nil {
		return f.take(ctx, p, opID)
	}
	return store.Backup{}, errors.New("fakeArchiver takes no new backups")
}

func (f *fakeArchiver) VerifyArchive(ctx context.Context, _ store.Project, b store.Backup, _ *jobs.StepLogger) (store.Backup, error) {
	f.verified = append(f.verified, b.ID)
	if f.fail != nil {
		return store.Backup{}, f.fail
	}
	return store.New(f.db).MarkBackupVerified(ctx, b.ID)
}
