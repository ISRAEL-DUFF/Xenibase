package alerts

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// A failed minor upgrade fires an alert for the instance until a later
// upgrade of it succeeds.
func TestMinorUpgradeFailedCondition(t *testing.T) {
	db := storetest.New(t)
	ctx := context.Background()
	q := store.New(db)
	n, err := q.UpsertNode(ctx, store.UpsertNodeParams{Name: "node1", PrivateAddr: "127.0.0.1", Role: "both"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := q.InsertInstance(ctx, store.InsertInstanceParams{ID: uuid.New(), NodeID: n.ID, Kind: "dedicated", PgVersion: 18})
	if err != nil {
		t.Fatal(err)
	}
	s := New(db, nil, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	minor := func() []condition {
		t.Helper()
		conds, err := s.conditions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []condition
		for _, c := range conds {
			if c.kind == KindMinorUpgradeFailed {
				out = append(out, c)
			}
		}
		return out
	}
	run := func(failure *string) {
		t.Helper()
		m, err := q.StartMinorUpgrade(ctx, store.StartMinorUpgradeParams{InstanceID: inst.ID, FromRelease: "18.1", ToRelease: "18.2"})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.FinishMinorUpgrade(ctx, store.FinishMinorUpgradeParams{ID: m.ID, Error: failure}); err != nil {
			t.Fatal(err)
		}
	}
	msg := "instance did not become ready; the old container was started again"
	run(&msg)
	if c := minor(); len(c) != 1 || c[0].targetID != inst.ID.String() || c[0].severity != SeverityCritical {
		t.Fatalf("after a failed upgrade: %+v", c)
	}
	run(nil)
	if c := minor(); len(c) != 0 {
		t.Fatalf("after a later success: %+v", c)
	}
}
