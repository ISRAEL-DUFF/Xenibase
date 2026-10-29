package services

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/files"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// Stored bytes used to be metered only for the projects the sweep reached
// (services on), so turning backend services off, which keeps the files,
// stopped their storage_gb_hours; cached renders were never measured. Now
// every live project with files is metered from its last measurement,
// renders included, and the sweep reaches projects whose services are off.
func TestStorageIsMeteredWhateverTheServicesState(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	p := storageTestProject(t, db, "metereds")
	if _, err := db.Exec(ctx, `UPDATE project_services SET enabled = false WHERE project_id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	q := store.New(db)
	ps, err := q.StorageProjects(ctx)
	if err != nil || len(ps) != 1 || ps[0].Project.ID != p.ID {
		t.Fatalf("the sweep's projects: %+v %v", ps, err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO project_storage (project_id, bytes, objects, transform_bytes) VALUES ($1, $2, 3, $3)`,
		p.ID, int64(1)<<30, int64(1)<<29); err != nil {
		t.Fatal(err)
	}
	s := &Service{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for range 2 { // a replace: an hour is metered once
		if err := s.meterStorage(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var gb float64
	if err := db.QueryRow(ctx, `SELECT coalesce(sum(quantity), 0)::float8 FROM usage_records WHERE project_id = $1 AND metric = 'storage_gb_hours'`,
		p.ID).Scan(&gb); err != nil {
		t.Fatal(err)
	}
	if gb < 1.6 || gb > 1.62 { // 1.5 GiB, in GB
		t.Fatalf("metered %v GB-hours", gb)
	}
}

// The reconciler measures what a project's renders take, for metering, and
// removes the renders of versions no row names.
func TestReconcilerMeasuresRenders(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	p := storageTestProject(t, db, "rendered")
	for _, sql := range []string{
		`CREATE SCHEMA xb_storage`,
		`CREATE TABLE xb_storage.objects (version uuid NOT NULL UNIQUE, bucket text NOT NULL, path text NOT NULL)`,
		`CREATE TABLE xb_storage.uploads (version uuid NOT NULL UNIQUE)`,
		`CREATE TABLE xb_storage.garbage (version uuid PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	f, cl := fakeFiles(t)
	kept, gone := uuid.New(), uuid.New()
	put := func(key string, n int) {
		t.Helper()
		if _, err := cl.Put(ctx, key, strings.NewReader(strings.Repeat("x", n)), int64(n), "image/png"); err != nil {
			t.Fatal(err)
		}
	}
	prefix := FilesPrefix("rendered")
	put(objectsKey("rendered", kept), 1000)
	put(files.TransformsPrefix(prefix, kept)+"/a.webp", 300)
	put(files.TransformsPrefix(prefix, kept)+"/b.png", 200)
	put(files.TransformsPrefix(prefix, gone)+"/a.webp", 999)
	if _, err := db.Exec(ctx, `INSERT INTO xb_storage.objects VALUES ($1, 'pics', 'a.png')`, kept); err != nil {
		t.Fatal(err)
	}
	s := &Service{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	connect := func() (*pgx.Conn, error) { return pgx.ConnectConfig(ctx, db.Config().ConnConfig) }
	if err := s.reconcileFiles(ctx, cl, p.ID, "rendered", connect, 0); err != nil {
		t.Fatal(err)
	}
	ps, err := store.New(db).GetProjectStorage(ctx, p.ID)
	if err != nil || ps.TransformBytes != 500 {
		t.Fatalf("renders measured: %+v %v", ps, err)
	}
	if keys, err := f.Objects("files"); err != nil || len(keys) != 3 {
		t.Fatalf("left: %v %v", keys, err)
	}
}

// The reconciler's last run used to be a local variable, so it ran only
// after a server had been up for a day: deployed more often, it never ran.
// The run is now claimed in the metadata database, once a day across
// servers and restarts.
func TestReconcilerRunsNightlyAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	fake, _ := fakeFiles(t)
	target := func(context.Context, string, bool) (storage.Target, error) { return fake.Target("files"), nil }
	server := func() *Service {
		return &Service{db: db, log: slog.New(slog.NewTextHandler(io.Discard, nil)), Files: target}
	}
	if ran, err := server().ReconcileStorageDue(ctx); err != nil || !ran {
		t.Fatalf("the first run: %v %v", ran, err)
	}
	// Another server, or this one restarted: not again today.
	if ran, err := server().ReconcileStorageDue(ctx); err != nil || ran {
		t.Fatalf("a restart: %v %v", ran, err)
	}
	if _, err := db.Exec(ctx, `UPDATE storage_reconciler SET last_run_at = now() - interval '25 hours'`); err != nil {
		t.Fatal(err)
	}
	if ran, err := server().ReconcileStorageDue(ctx); err != nil || !ran {
		t.Fatalf("a day on: %v %v", ran, err)
	}
}
