package provision

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A project's placement holds its node's row until it is recorded, so the
// empty-node sweep (capacity automation), which takes an empty node out
// of placement under that row's lock, either counts the project or has
// taken the node out first. Here the sweep holds the lock when the project
// has already chosen the node's cluster: the creation waits, sees the node
// retiring, and finds no other cluster rather than landing on it.
func TestPlacementWaitsForTheEmptyNodeSweep(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM nodes WHERE id = $1 FOR UPDATE`, h.inst.NodeID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.s.Create(ctx, CreateParams{OrgID: h.org, Name: "late"})
		done <- err
	}()
	// The creation picks the cluster, then waits for the node's row.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var waiting bool
		if err := h.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the project was placed without waiting for the node's lock (err %v)", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the creation never waited for the node's lock")
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE nodes SET lifecycle = 'retiring', retiring_from = 'active', retiring_at = now() WHERE id = $1`, h.inst.NodeID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("creating on a node retired meanwhile: %v, want no capacity", err)
	}
	var n int
	if err := h.db.QueryRow(ctx, `SELECT count(*) FROM projects WHERE instance_id = $1`, h.inst.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d project(s) on the retiring node's cluster (%v)", n, err)
	}
}
