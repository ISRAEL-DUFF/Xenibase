package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// harness is a Service against a throwaway metadata database, with
// in-memory S3 buckets for its storage targets. It has no agents: it covers
// the bookkeeping around them.
type harness struct {
	s   *Service
	db  *pgxpool.Pool
	org uuid.UUID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db := storetest.New(t)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	kr, err := crypto.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := agentca.LoadOrCreate(ctx, db, kr)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := nodes.NewService(db, ca, "", quiet)
	if err != nil {
		t.Fatal(err)
	}
	ps := provision.NewService(db, kr, nil, provision.Config{}, quiet)
	h := &harness{s: NewService(db, kr, ns, ps, Config{}, quiet), db: db}
	if err := db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id)
		SELECT 'Test', 'test', id FROM quota_plans ORDER BY created_at LIMIT 1 RETURNING id`).Scan(&h.org); err != nil {
		t.Fatal(err)
	}
	return h
}

// bucket starts an in-memory S3 server holding one bucket and returns a
// target for it.
func (h *harness) bucket(t *testing.T, name string) storage.Target {
	t.Helper()
	f, err := storage.NewFake(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f.Target(name)
}

// target saves a platform target (org nil) or an org target after its live
// test, as the admin API does.
func (h *harness) target(t *testing.T, org *uuid.UUID, name string, st storage.Target) store.StorageTarget {
	t.Helper()
	row, _, err := h.s.SaveTarget(context.Background(), org, nil, input(name, st), nil)
	if err != nil {
		t.Fatalf("save target %s: %v", name, err)
	}
	return row
}

func input(name string, st storage.Target) TargetInput {
	return TargetInput{
		Name: name, Endpoint: st.Endpoint, Region: st.Region, Bucket: st.Bucket, Prefix: st.Prefix,
		AccessKey: st.AccessKey, SecretKey: st.SecretKey, PathStyle: st.PathStyle,
	}
}

// project adds a shared project on a cluster of its own.
func (h *harness) project(t *testing.T, name string) store.Project {
	t.Helper()
	ctx := context.Background()
	q := store.New(h.db)
	node, err := q.UpsertNode(ctx, store.UpsertNodeParams{Name: "node-" + name, PrivateAddr: "127.0.0.1", Role: "both"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := q.InsertInstance(ctx, store.InsertInstanceParams{ID: uuid.New(), NodeID: node.ID, Kind: "shared", PgVersion: 18})
	if err != nil {
		t.Fatal(err)
	}
	p, err := q.InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: h.org, Name: name, Slug: name, DbName: "p_" + name, OwnerRole: "p_" + name + "_owner",
		ScramVerifier: "SCRAM-SHA-256$4096:x$y:z", Tier: "shared", InstanceID: inst.ID, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetProjectStatus(ctx, store.SetProjectStatusParams{ID: p.ID, Status: provision.StatusActive}); err != nil {
		t.Fatal(err)
	}
	p.Status = provision.StatusActive
	return p
}

// backup records a finished backup of p on target, its object holding body.
func (h *harness) backup(t *testing.T, p store.Project, kind string, target store.StorageTarget, st storage.Target, body string) store.Backup {
	t.Helper()
	ctx := context.Background()
	q := store.New(h.db)
	key := objectKey(kind, &p.ID, time.Now(), false)
	row, err := q.InsertBackup(ctx, store.InsertBackupParams{ProjectID: &p.ID, Kind: kind, ObjectKey: key, StorageTargetID: &target.ID, KeyWrapped: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	put(t, st, key, body)
	h256 := sha256.Sum256([]byte(body))
	sum := hex.EncodeToString(h256[:])
	if row, err = q.FinishBackup(ctx, store.FinishBackupParams{ID: row.ID, SizeBytes: ptr(int64(len(body))), Checksum: &sum}); err != nil {
		t.Fatal(err)
	}
	return row
}

// put stores body under the target's key for rel.
func put(t *testing.T, st storage.Target, rel, body string) {
	t.Helper()
	c, err := storage.New(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(context.Background(), rel, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

// exists reports whether rel is stored under the target.
func exists(t *testing.T, st storage.Target, rel string) bool {
	t.Helper()
	c, err := storage.New(st)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Download(context.Background(), rel)
	if err != nil {
		return false
	}
	_ = r.Close()
	return true
}

func status(t *testing.T, h *harness, id uuid.UUID) string {
	t.Helper()
	b, err := store.New(h.db).GetBackup(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Status
}

// timeAgo is h hours ago.
func timeAgo(h int) time.Time { return time.Now().Add(-time.Duration(h) * time.Hour) }
