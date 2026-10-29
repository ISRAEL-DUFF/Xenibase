package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool := storetest.New(t)
	if err := store.Migrate(context.Background(), pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
}

func TestClaimSkipsLockedAndFiltersKinds(t *testing.T) {
	ctx := context.Background()
	q := store.New(storetest.New(t))

	a, err := q.EnqueueOperation(ctx, store.EnqueueOperationParams{Kind: "noop", Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.EnqueueOperation(ctx, store.EnqueueOperationParams{Kind: "other", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}

	got, err := q.ClaimOperation(ctx, store.ClaimOperationParams{Worker: "w1", Kinds: []string{"noop"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != a.ID || got.Status != "running" || got.Attempts != 1 || got.LockedBy == nil || *got.LockedBy != "w1" {
		t.Fatalf("unexpected claim: %+v", got)
	}
	if _, err := q.ClaimOperation(ctx, store.ClaimOperationParams{Worker: "w2", Kinds: []string{"noop"}}); err == nil {
		t.Fatal("second claim should find nothing")
	}

	// Only the lock holder may write.
	if n, _ := q.SucceedOperation(ctx, store.SucceedOperationParams{ID: a.ID, Worker: "w2", Attempt: 1}); n != 0 {
		t.Fatal("non-owner completed the operation")
	}
	// Nor may an earlier attempt of the same worker.
	if n, _ := q.SucceedOperation(ctx, store.SucceedOperationParams{ID: a.ID, Worker: "w1", Attempt: 0}); n != 0 {
		t.Fatal("a stale attempt completed the operation")
	}
	if n, _ := q.AppendOperationLog(ctx, store.AppendOperationLogParams{ID: a.ID, Worker: "w1", Attempt: 1, Entry: json.RawMessage(`{"msg":"hi"}`)}); n != 1 {
		t.Fatal("owner could not append log")
	}
	if n, _ := q.SucceedOperation(ctx, store.SucceedOperationParams{ID: a.ID, Worker: "w1", Attempt: 1}); n != 1 {
		t.Fatal("owner could not complete")
	}
	done, err := q.GetOperation(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "succeeded" || done.FinishedAt == nil || done.LockedBy != nil {
		t.Fatalf("unexpected final state: %+v", done)
	}
}

func TestReclaimStale(t *testing.T) {
	ctx := context.Background()
	q := store.New(storetest.New(t))

	op, _ := q.EnqueueOperation(ctx, store.EnqueueOperationParams{Kind: "noop", Params: json.RawMessage(`{}`)})
	if _, err := q.ClaimOperation(ctx, store.ClaimOperationParams{Worker: "dead", Kinds: []string{"noop"}}); err != nil {
		t.Fatal(err)
	}

	ids, err := q.ReclaimStaleOperations(ctx, -60)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != op.ID {
		t.Fatalf("reclaimed %v", ids)
	}
	got, _ := q.GetOperation(ctx, op.ID)
	if got.Status != "queued" || got.LockedBy != nil || string(got.Log) == "[]" {
		t.Fatalf("unexpected state after reclaim: %+v log=%s", got, got.Log)
	}
}

// The lease cutoff is measured on the database's clock, which wrote
// locked_at, so a worker whose clock runs ahead reclaims nothing early.
func TestReclaimUsesDatabaseClock(t *testing.T) {
	ctx := context.Background()
	pool := storetest.New(t)
	q := store.New(pool)
	lease := (30 * time.Second).Seconds()

	op, _ := q.EnqueueOperation(ctx, store.EnqueueOperationParams{Kind: "noop", Params: json.RawMessage(`{}`)})
	if _, err := q.ClaimOperation(ctx, store.ClaimOperationParams{Worker: "w", Kinds: []string{"noop"}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		age  string
		want int
	}{{"10 seconds", 0}, {"40 seconds", 1}} {
		if _, err := pool.Exec(ctx, `UPDATE operations SET locked_at = now() - $2::interval WHERE id = $1`, op.ID, c.age); err != nil {
			t.Fatal(err)
		}
		ids, err := q.ReclaimStaleOperations(ctx, lease)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != c.want {
			t.Fatalf("lease held for %s: reclaimed %d, want %d", c.age, len(ids), c.want)
		}
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	pool := storetest.New(t)
	q := store.New(pool)
	if err := q.InsertAudit(ctx, store.InsertAuditParams{Action: "test.append", Detail: json.RawMessage(`{}`), Outcome: "success", ActorKind: "system"}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE audit_log SET action = 'test.changed'`,
		`DELETE FROM audit_log`,
		`TRUNCATE audit_log`,
	} {
		if _, err := pool.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v", stmt, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'test.append'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows: %d %v", n, err)
	}
}

// A release must not run on a database a newer one migrated: its queries
// don't know the newer schema, and nothing says so until data is wrong.
func TestMigrateRefusesANewerSchema(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewEmpty(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool, log); err != nil {
		t.Fatalf("migrating an up-to-date database: %v", err)
	}
	// A later release applied a migration this one doesn't have.
	if _, err := pool.Exec(ctx, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (9999, true)`); err != nil {
		t.Fatal(err)
	}
	err := store.Migrate(ctx, pool, log)
	if !errors.Is(err, store.ErrSchemaNewer) {
		t.Fatalf("Migrate on a newer schema = %v, want ErrSchemaNewer", err)
	}
	if !strings.Contains(err.Error(), "9999") || !strings.Contains(err.Error(), "restore the metadata backup") {
		t.Errorf("the message should say what to do: %v", err)
	}
}
