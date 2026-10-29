package tenancy_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// cluster is a metadata database whose one shared cluster is the test
// Postgres server (XENIBASE_TEST_DATABASE_URL, a superuser), with a
// provision and a tenancy service over it.
type cluster struct {
	db       *pgxpool.Pool
	admin    *pgx.Conn // superuser on the cluster
	cfg      *pgx.ConnConfig
	projects *provision.Service
	inst     uuid.UUID
	org      uuid.UUID
}

func newCluster(t *testing.T, cfg tenancy.Config) (*cluster, *tenancy.Service) {
	t.Helper()
	ctx := context.Background()
	db := storetest.New(t)
	pcfg, err := pgx.ParseConfig(os.Getenv(storetest.EnvVar))
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.ConnectConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	key, _ := crypto.GenerateKey()
	kr, err := crypto.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	q := store.New(db)
	if err := q.EnsureRegion(ctx, store.EnsureRegionParams{ID: "eu-central", Name: "eu-central"}); err != nil {
		t.Fatal(err)
	}
	node, err := q.UpsertNode(ctx, store.UpsertNodeParams{Name: "test", PrivateAddr: pcfg.Host, Role: "both"})
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(provision.AdminSecret{User: pcfg.User, Password: pcfg.Password})
	sealed, err := kr.Encrypt(secret, []byte("nodes.pg_admin_secret:"+node.ID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetNodeAdminSecret(ctx, store.SetNodeAdminSecretParams{ID: node.ID, PgAdminSecret: sealed}); err != nil {
		t.Fatal(err)
	}
	var major int32
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int / 10000`).Scan(&major); err != nil {
		t.Fatal(err)
	}
	host, port := pcfg.Host, int32(pcfg.Port)
	inst, err := q.UpsertSharedInstance(ctx, store.UpsertSharedInstanceParams{
		NodeID: node.ID, PgVersion: major, Port: port, AdminHost: &host, AdminPort: &port,
	})
	if err != nil {
		t.Fatal(err)
	}
	var org uuid.UUID
	if err := db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id, settings)
		SELECT 'Test', 'test-' || substr(md5(random()::text), 1, 8), id, '{}' FROM quota_plans ORDER BY created_at LIMIT 1
		RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ps := provision.NewService(db, kr, nil, provision.Config{HomeRegion: "eu-central", AdminSSLMode: "disable"}, log)
	return &cluster{db: db, admin: admin, cfg: pcfg, projects: ps, inst: inst.ID, org: org}, tenancy.New(db, ps, nil, cfg, log)
}

// tenant is one shared project: a database and an owner login on the
// cluster, and its projects row.
type tenant struct {
	id   uuid.UUID
	db   string
	conn *pgx.ConnConfig // as the owner
}

func (c *cluster) tenant(t *testing.T) tenant {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	name := "p_tentest" + hex.EncodeToString(b)
	role := name + "_owner"
	id := provision.Ident
	t.Cleanup(func() {
		c := context.Background()
		admin, err := pgx.Connect(c, os.Getenv(storetest.EnvVar))
		if err != nil {
			return
		}
		defer admin.Close(c)
		_, _ = admin.Exec(c, "DROP DATABASE IF EXISTS "+id(name)+" WITH (FORCE)")
		_, _ = admin.Exec(c, "DROP ROLE IF EXISTS "+id(role))
	})
	for _, q := range []string{
		"CREATE ROLE " + id(role) + " LOGIN PASSWORD 'tenant-pw'",
		"CREATE DATABASE " + id(name) + " OWNER " + id(role),
	} {
		if _, err := c.admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var p uuid.UUID
	if err := c.db.QueryRow(ctx, `INSERT INTO projects (org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, region)
		VALUES ($1, $2, $2, $2, $3, 'x', 'shared', $4, 'active', 'eu-central') RETURNING id`, c.org, name, role, c.inst).Scan(&p); err != nil {
		t.Fatal(err)
	}
	tc := c.cfg.Copy()
	tc.Database, tc.User, tc.Password = name, role, "tenant-pw"
	return tenant{id: p, db: name, conn: tc}
}
