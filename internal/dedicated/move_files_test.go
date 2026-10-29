package dedicated_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// A move to another region's node used to leave every stored file behind:
// the bytes stay in the old region's object store, the edge looks in the
// new region's, and nothing copies them. A region move is now refused
// while xb_storage.objects (or an upload in progress) has rows; a move
// within the region is not affected.
func TestRegionMoveRefusesAProjectWithFiles(t *testing.T) {
	e := setupMove(t, "active")
	ctx := context.Background()
	q := store.New(e.db)
	p := e.project
	if _, err := e.cluster.Exec(ctx, "CREATE DATABASE "+p.DbName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.cluster.Exec(context.Background(), "DROP DATABASE IF EXISTS "+p.DbName+" WITH (FORCE)")
	})
	cfg, err := pgx.ParseConfig(os.Getenv(storetest.EnvVar))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = p.DbName
	pdb, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pdb.Close(ctx)
	for _, sql := range []string{
		`CREATE SCHEMA xb_storage`,
		`CREATE TABLE xb_storage.objects (bucket text, path text)`,
		`CREATE TABLE xb_storage.uploads (bucket text, path text)`,
		`INSERT INTO xb_storage.objects VALUES ('docs', 'a.pdf')`,
	} {
		if _, err := pdb.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.db.Exec(ctx, `INSERT INTO project_services (project_id, ref, enabled, schema_version) VALUES ($1, 'mfiles01', true, 4)`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.EnsureRegion(ctx, store.EnsureRegionParams{ID: "lagos", Name: "lagos"}); err != nil {
		t.Fatal(err)
	}
	far := e.node(t, "far", "healthy")
	if _, err := e.db.Exec(ctx, `UPDATE nodes SET region = 'lagos' WHERE id = $1`, far.ID); err != nil {
		t.Fatal(err)
	}
	move := func() error {
		_, err := e.svc.Move(ctx, dedicated.MoveParams{ProjectID: p.ID, NodeID: far.ID})
		return err
	}
	if err := move(); !errors.Is(err, provision.ErrConflict) || !strings.Contains(err.Error(), "stored files") {
		t.Fatalf("a region move with files: %v", err)
	}
	if _, err := pdb.Exec(ctx, `DELETE FROM xb_storage.objects`); err != nil {
		t.Fatal(err)
	}
	if _, err := pdb.Exec(ctx, `INSERT INTO xb_storage.uploads VALUES ('docs', 'big.bin')`); err != nil {
		t.Fatal(err)
	}
	if err := move(); err == nil || !strings.Contains(err.Error(), "stored files") {
		t.Fatalf("a region move with an upload in progress: %v", err)
	}
	if _, err := pdb.Exec(ctx, `DELETE FROM xb_storage.uploads`); err != nil {
		t.Fatal(err)
	}
	// Without files the move goes past the check (whatever it meets next).
	if err := move(); err != nil && strings.Contains(err.Error(), "stored files") {
		t.Fatalf("a region move without files: %v", err)
	}
}
