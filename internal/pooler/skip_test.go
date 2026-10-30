package pooler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// TestSyncSkipsABadRoute: a node address the renderer refuses (pg_shared,
// with an underscore) used to make Render refuse the whole configuration,
// so no region's files changed. Sync now leaves that project's route out,
// says why, and writes everyone else's. Needs XENIBASE_TEST_DATABASE_URL.
func TestSyncSkipsABadRoute(t *testing.T) {
	ctx := context.Background()
	pool := storetest.New(t)
	good, bad, goodInst, badInst, org := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, stmt := range []string{
		`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ('` + good.String() + `', 'n1', '10.0.0.1', 'both', 'fp', '{}')`,
		// Written straight to the table, as before CreateNode checked it.
		`INSERT INTO nodes (id, name, private_addr, role, agent_cert_fp, capacity) VALUES ('` + bad.String() + `', 'n2', 'pg_shared', 'both', 'fp2', '{}')`,
		`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ('` + goodInst.String() + `', '` + good.String() + `', 'shared', 18, 5432, 'running')`,
		`INSERT INTO instances (id, node_id, kind, pg_version, port, status) VALUES ('` + badInst.String() + `', '` + bad.String() + `', 'shared', 18, 5432, 'running')`,
		`INSERT INTO organizations (id, name, slug, plan_id) VALUES ('` + org.String() + `', 'o', 'o', (SELECT id FROM quota_plans WHERE name = 'Unlimited'))`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for _, p := range []struct {
		db   string
		inst uuid.UUID
	}{{"p_aaaaaaaaaa", goodInst}, {"p_bbbbbbbbbb", badInst}} {
		if _, err := pool.Exec(ctx, `INSERT INTO projects (id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, org_id, region)
			VALUES ($1, $2::text, $2::text, $2::text, $2::text || '_owner', 'SCRAM-SHA-256$4096:c2FsdA==$a2V5:a2V5', 'shared', $3, 'active', $4, 'eu-central')`,
			uuid.New(), p.db, p.inst, org); err != nil {
			t.Fatalf("project %s: %v", p.db, err)
		}
	}
	dir := t.TempDir()
	m, err := NewManager(dir, 0o640, pool, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Sync(ctx); err != nil {
		t.Fatalf("one bad route failed the whole sync: %v", err)
	}
	routes, err := os.ReadFile(filepath.Join(dir, DatabasesFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(routes), "p_aaaaaaaaaa = host=10.0.0.1 port=5432") {
		t.Fatalf("the good project's route is missing:\n%s", routes)
	}
	if strings.Contains(string(routes), "pg_shared") || strings.Contains(string(routes), "p_bbbbbbbbbb =") {
		t.Fatalf("the bad route was written:\n%s", routes)
	}
	if s := m.Skipped(); len(s) != 1 || !strings.Contains(s[0], `"pg_shared"`) {
		t.Fatalf("skipped: %q", s)
	}
}

// TestDropInvalidFailsClosed: a bad allowlist takes its project's routes
// out instead of leaving the project open to everyone.
func TestDropInvalidFailsClosed(t *testing.T) {
	cfg := Config{
		Routes: []Route{{Database: "p_aaaaaaaaaa", Host: "10.0.0.1", Port: 5432}, {Database: "p_aaaaaaaaaa_ro", BackendDB: "p_aaaaaaaaaa", Host: "10.0.0.1", Port: 5432},
			{Database: "p_bbbbbbbbbb", Host: "10.0.0.1", Port: 5432}, {Database: "p_bbbbbbbbbb", Host: "10.0.0.2", Port: 5432}},
		Users:        []User{{Name: "p_aaaaaaaaaa_owner", Secret: verifier}, {Name: "bad name", Secret: verifier}},
		Restrictions: []Restriction{{Databases: []string{"p_aaaaaaaaaa", "p_aaaaaaaaaa_ro"}, CIDRs: []string{"10.0.0.1/8"}}},
	}
	errs := cfg.DropInvalid()
	if len(errs) != 5 { // the list, its two routes, the duplicate, the user
		t.Fatalf("errors: %v", errs)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Database != "p_bbbbbbbbbb" || cfg.Routes[0].Host != "10.0.0.1" || len(cfg.Users) != 1 || len(cfg.Restrictions) != 0 {
		t.Fatalf("kept %+v", cfg)
	}
	if _, _, _, err := Render(cfg); err != nil {
		t.Fatalf("what DropInvalid keeps must render: %v", err)
	}
}
