package services

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// storageTestProject is an active project with backend services (ref) in
// a fresh metadata database.
func storageTestProject(t *testing.T, db *pgxpool.Pool, ref string) store.Project {
	t.Helper()
	ctx := context.Background()
	q := store.New(db)
	var org uuid.UUID
	if err := db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id)
		SELECT 'Files', 'files-'||$1::text, id FROM quota_plans WHERE name = 'Personal' RETURNING id`, ref).Scan(&org); err != nil {
		t.Fatal(err)
	}
	node, err := q.UpsertNode(ctx, store.UpsertNodeParams{Name: "node-" + ref, PrivateAddr: "127.0.0.1", Role: "both"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := q.InsertInstance(ctx, store.InsertInstanceParams{ID: uuid.New(), NodeID: node.ID, Kind: "shared", PgVersion: 18})
	if err != nil {
		t.Fatal(err)
	}
	p, err := q.InsertProject(ctx, store.InsertProjectParams{Region: "eu-central", ID: uuid.New(), OrgID: org, Name: "files " + ref,
		Slug: "files-" + ref, DbName: "db_" + ref, OwnerRole: "db_" + ref + "_owner", ScramVerifier: "SCRAM-SHA-256$4096:x$y:z",
		Tier: "shared", InstanceID: inst.ID, Settings: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO project_services (project_id, ref, enabled, schema_version) VALUES ($1, $2, true, 4)`, p.ID, ref); err != nil {
		t.Fatal(err)
	}
	p, err = q.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// flakyCDN records purges, refusing while down.
type flakyCDN struct {
	mu     sync.Mutex
	down   bool
	purged []string
}

func (f *flakyCDN) PurgePrefixes(_ context.Context, ps []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return errors.New("cloudflare: 503")
	}
	f.purged = append(f.purged, ps...)
	return nil
}

func (f *flakyCDN) set(down bool) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
	return append([]string(nil), f.purged...)
}

// A CDN purge used to be tried once, and only for a bucket made private:
// a failed one was only logged, and deleting, replacing or moving a public
// bucket's object, or deleting the bucket, purged nothing. Purges are now
// queued in the metadata database and the sweep retries each until the CDN
// takes it, and the edge's object and bucket events purge too.
func TestCDNPurgesAreQueuedAndRetried(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)
	p := storageTestProject(t, db, "cdnpurge")
	cdn := &flakyCDN{down: true}
	s := &Service{db: db, cfg: Config{Domain: "api.test"}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), CDN: cdn}

	// An object deleted from a public bucket while the CDN refuses.
	if err := s.StorageEvent(ctx, edgeapi.StorageEvent{Ref: "cdnpurge", Event: edgeapi.EventObjectsChanged, Bucket: "site",
		Paths: []string{"img/logo one.png"}}); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM cdn_purges WHERE project_id = $1 AND attempts = 1`, p.ID).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued: %d %v", queued, err)
	}
	// Not due yet: the sweep leaves it.
	if err := s.RetryCDNPurges(ctx); err != nil {
		t.Fatal(err)
	}
	if got := cdn.set(false); len(got) != 0 {
		t.Fatalf("purged before its time: %v", got)
	}
	if _, err := db.Exec(ctx, `UPDATE cdn_purges SET next_at = now()`); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryCDNPurges(ctx); err != nil {
		t.Fatal(err)
	}
	got := cdn.set(false)
	want := "cdnpurge.api.test/storage/v1/public/site/img/logo%20one.png"
	if len(got) != len(publicRoutes) || got[0] != want {
		t.Fatalf("purged: %v", got)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM cdn_purges`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("left queued: %d %v", queued, err)
	}

	// A deleted public bucket goes whole, at once when the CDN answers.
	if err := s.StorageEvent(ctx, edgeapi.StorageEvent{Ref: "cdnpurge", Event: edgeapi.EventBucketDeleted, Bucket: "site"}); err != nil {
		t.Fatal(err)
	}
	got = cdn.set(false)
	if !strings.HasSuffix(got[len(got)-1], "/storage/v1/render/site/") {
		t.Fatalf("purged: %v", got)
	}
	// Many objects purge the bucket rather than each.
	many := make([]string, purgeEachUpTo+1)
	for i := range many {
		many[i] = uuid.NewString()
	}
	if err := s.StorageEvent(ctx, edgeapi.StorageEvent{Ref: "cdnpurge", Event: edgeapi.EventObjectsChanged, Bucket: "site", Paths: many}); err != nil {
		t.Fatal(err)
	}
	if got2 := cdn.set(false); len(got2) != len(got)+len(publicRoutes) {
		t.Fatalf("purged for %d objects: %v", len(many), got2[len(got):])
	}
	if err := s.StorageEvent(ctx, edgeapi.StorageEvent{Ref: "cdnpurge", Event: edgeapi.EventObjectsChanged, Bucket: "site",
		Paths: []string{"../x"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad path: %v", err)
	}
}
