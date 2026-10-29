package branching

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/authz"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// harness is a branching Service against a throwaway metadata database,
// with the test Postgres server (XENIBASE_TEST_DATABASE_URL, a superuser)
// as its one shared cluster and poolers that are only configuration files.
type harness struct {
	s       *Service
	db      *pgxpool.Pool
	cluster *pgx.Conn
	inst    store.Instance
	org     uuid.UUID
	now     time.Time
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

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
	ps := provision.NewService(db, keyring, pm, provision.Config{
		HomeRegion: "eu-central", DBHost: "localhost", SessionPort: 5432, PooledPort: 6543,
		AdminSSLMode: "disable", PGVersions: []int{major},
	}, quiet)
	h := &harness{db: db, cluster: cluster, inst: inst, org: org, now: time.Now()}
	h.s = New(db, ps, nil, nil, nil, Config{Now: func() time.Time { return h.now }}, quiet)
	return h
}

// user adds an account to the harness's organisation with orgRole.
func (h *harness) user(t *testing.T, orgRole string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	if _, err := h.db.Exec(ctx, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, id, id.String()+"@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := store.New(h.db).InsertOrgMember(ctx, store.InsertOrgMemberParams{OrgID: h.org, UserID: id, Role: orgRole}); err != nil {
		t.Fatal(err)
	}
	return id
}

// project records an active project (a branch of parent when set) without
// anything on the cluster.
func (h *harness) project(t *testing.T, name string, parent *store.Project) store.Project {
	t.Helper()
	ctx := context.Background()
	q := store.New(h.db)
	slug := strings.ToLower(name) + "-" + uuid.NewString()[:6]
	db := "p_" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
	p, err := q.InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: h.org, Name: name, Slug: slug, DbName: db, OwnerRole: db + "_owner",
		ScramVerifier: "x", Tier: provision.TierShared, InstanceID: h.inst.ID, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if parent != nil {
		src, schemaOnly := SourceLive, false
		if err := q.SetProjectBranch(ctx, store.SetProjectBranchParams{ID: p.ID, ParentProjectID: &parent.ID, BranchSource: &src,
			BranchSchemaOnly: &schemaOnly, SensitiveData: parent.SensitiveData}); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: provision.StatusActive}); err != nil {
		t.Fatal(err)
	}
	p, err = q.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// member gives userID role on p.
func (h *harness) member(t *testing.T, p store.Project, userID uuid.UUID, role string) {
	t.Helper()
	if err := store.New(h.db).UpsertProjectMember(context.Background(), store.UpsertProjectMemberParams{ProjectID: p.ID, UserID: userID, OrgID: p.OrgID, Role: role}); err != nil {
		t.Fatal(err)
	}
}

// idle forgets p's pending operations and makes it active again.
func (h *harness) idle(t *testing.T, p store.Project) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.db.Exec(ctx, `DELETE FROM operations WHERE project_id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(ctx, `UPDATE projects SET status = 'active' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) get(t *testing.T, id uuid.UUID) store.Project {
	t.Helper()
	p, err := store.New(h.db).GetProject(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
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

func session(userID uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.ActorSession, UserID: userID}
}
