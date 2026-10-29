package dedicated_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
)

// TestInstancesBehindSkipsAFailedUpgrade: an instance whose last upgrade
// to its image's release failed is left out, so the sweep moves on to the
// next one; a newer release brings it back.
func TestInstancesBehindSkipsAFailedUpgrade(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	q := store.New(e.db)
	node := e.node(t, "node1", "healthy")
	failed, next := e.instance(t, node, "dedicated", "2 hours"), e.instance(t, node, "dedicated", "1 hour")
	for _, inst := range []store.Instance{failed, next} {
		if _, err := e.db.Exec(ctx, `UPDATE instances SET status = 'running', pg_release = '18.1', pg_release_available = '18.2' WHERE id = $1`, inst.ID); err != nil {
			t.Fatal(err)
		}
	}
	behind := func() []uuid.UUID {
		t.Helper()
		list, err := q.InstancesBehind(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for _, i := range list {
			ids = append(ids, i.ID)
		}
		return ids
	}
	if ids := behind(); len(ids) != 2 || ids[0] != failed.ID {
		t.Fatalf("behind before any upgrade: %v", ids)
	}
	m, err := q.StartMinorUpgrade(ctx, store.StartMinorUpgradeParams{InstanceID: failed.ID, FromRelease: "18.1", ToRelease: "18.2"})
	if err != nil {
		t.Fatal(err)
	}
	msg := "the new container did not start"
	if err := q.FinishMinorUpgrade(ctx, store.FinishMinorUpgradeParams{ID: m.ID, Error: &msg}); err != nil {
		t.Fatal(err)
	}
	if ids := behind(); len(ids) != 1 || ids[0] != next.ID {
		t.Fatalf("behind after a failed upgrade: %v, want only the next instance", ids)
	}
	if _, err := e.db.Exec(ctx, `UPDATE instances SET pg_release_available = '18.3' WHERE id = $1`, failed.ID); err != nil {
		t.Fatal(err)
	}
	if ids := behind(); len(ids) != 2 {
		t.Fatalf("behind with a newer release: %v", ids)
	}
}
