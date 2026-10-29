package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/jobs"
)

// However it is queued, a project has one exclusive operation pending at a
// time: the schema refuses a second. A background one may be queued
// beside it.
func TestOneExclusiveOperationPerProject(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, _ := h.create(t) // its create operation is queued
	if _, err := jobs.Enqueue(ctx, h.db, jobs.EnqueueParams{Kind: KindApplySettings, ProjectID: &p.ID}); !errors.Is(err, jobs.ErrProjectBusy) {
		t.Fatalf("a second exclusive operation beside the create: %v", err)
	}
	if _, err := jobs.Enqueue(ctx, h.db, jobs.EnqueueParams{Kind: KindDropDBUser, ProjectID: &p.ID, Background: true}); err != nil {
		t.Fatalf("a background operation beside the create: %v", err)
	}
}

// The nightly scheduler's path queues only on an idle, active project,
// checked under the row lock.
func TestEnqueueIfIdleTx(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, create := h.create(t)
	if _, err := h.db.Exec(ctx, `UPDATE projects SET status = 'active' WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	try := func() bool {
		var queued bool
		if err := pgx.BeginFunc(ctx, h.db, func(tx pgx.Tx) error {
			var err error
			queued, err = EnqueueIfIdleTx(ctx, tx, jobs.EnqueueParams{Kind: "backup", ProjectID: &p.ID})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return queued
	}
	if try() {
		t.Fatal("queued beside the pending create")
	}
	if _, err := h.db.Exec(ctx, `UPDATE operations SET status = 'succeeded' WHERE id = $1`, create.ID); err != nil {
		t.Fatal(err)
	}
	if !try() {
		t.Fatal("an idle project was skipped")
	}
	if try() {
		t.Fatal("queued twice")
	}
}
