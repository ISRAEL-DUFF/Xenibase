package provision

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/jobs"
	"github.com/israel-duff/xenibase/internal/store"
)

// A failed create whose rollback cannot reach the cluster queues a cleanup
// that retries until the database and roles are gone.
func TestFailedCreateRollbackIsRetried(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, op := h.create(t)
	op, log := h.claim(t, op)

	// The shared cluster stops answering.
	if _, err := h.db.Exec(ctx, `UPDATE instances SET admin_port = 1 WHERE id = $1`, h.inst.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.s.rollbackCreate(ctx, op, log, errors.New("smoke test failed")); err == nil {
		t.Fatal("rollback reached a cluster that does not answer")
	}
	ops, err := store.New(h.db).ListOperations(ctx, store.ListOperationsParams{Kind: ptr(KindDropProjectObjects), ProjectID: &p.ID, MaxRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Status != jobs.StatusQueued {
		t.Fatalf("no cleanup queued after a failed rollback: %d operation(s)", len(ops))
	}
	if !h.databaseExists(t, p.DbName) {
		t.Fatal("database already gone")
	}

	// The cluster answers again; the cleanup drops what the rollback left.
	if _, err := h.db.Exec(ctx, `UPDATE instances SET admin_port = $2 WHERE id = $1`, h.inst.ID, *h.inst.AdminPort); err != nil {
		t.Fatal(err)
	}
	cleanup, clog := h.claim(t, ops[0])
	if err := h.s.Kinds()[KindDropProjectObjects].Handler(ctx, cleanup, clog); err != nil {
		t.Fatal(err)
	}
	if h.databaseExists(t, p.DbName) || h.roleExists(t, p.OwnerRole) {
		t.Fatal("cleanup left the database or the owner role")
	}
}

// The cleanup never touches a live project.
func TestDropProjectObjectsRefusesLiveProject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, _ := h.create(t)
	op, err := jobs.Enqueue(ctx, h.db, jobs.EnqueueParams{Kind: KindDropProjectObjects, ProjectID: &p.ID})
	if err != nil {
		t.Fatal(err)
	}
	op, log := h.claim(t, op)
	if err := h.s.Kinds()[KindDropProjectObjects].Handler(ctx, op, log); !jobs.IsPermanent(err) {
		t.Fatalf("cleanup of a live project: %v", err)
	}
	if !h.databaseExists(t, p.DbName) {
		t.Fatal("cleanup dropped a live project's database")
	}
}

func ptr[T any](v T) *T { return &v }

// The sweep drops the database and roles of a project deleted longer ago
// than the grace period, and leaves live, recent and unknown ones alone.
func TestSweepOrphans(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	live, _ := h.create(t)
	old, _ := h.create(t)
	recent, _ := h.create(t)
	for p, age := range map[uuid.UUID]string{old.ID: "48 hours", recent.ID: "1 hour"} {
		// A create that failed and whose rollback could not finish.
		if _, err := h.db.Exec(ctx, `UPDATE projects SET status = 'error', deleted_at = now() - $2::interval WHERE id = $1`, p, age); err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.Exec(ctx, `UPDATE operations SET status = 'failed' WHERE project_id = $1`, p); err != nil {
			t.Fatal(err)
		}
	}
	stranger, err := OpaqueDBName()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.cluster.Exec(ctx, "CREATE DATABASE "+ident(stranger)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.dropObjects(stranger) })

	rep, err := h.s.SweepOrphans(ctx, OrphanGrace)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Dropped, []string{old.DbName}) {
		t.Fatalf("dropped %v, want only %s", rep.Dropped, old.DbName)
	}
	if !slices.Contains(rep.Unknown, stranger) {
		t.Fatalf("unknown %v does not report %s", rep.Unknown, stranger)
	}
	if h.databaseExists(t, old.DbName) || h.roleExists(t, old.OwnerRole) {
		t.Fatal("the old orphan's database or owner role survived")
	}
	for _, db := range []string{live.DbName, recent.DbName, stranger} {
		if !h.databaseExists(t, db) {
			t.Fatalf("sweep dropped %s", db)
		}
	}
}
