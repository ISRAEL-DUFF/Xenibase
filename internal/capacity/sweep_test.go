package capacity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
)

// emptyNode adds a shared node with a provider server and a running shared
// cluster, empty since a day and an hour ago.
func (h *harness) emptyNode(t *testing.T, name string) (store.Node, store.Instance) {
	t.Helper()
	ctx := context.Background()
	h.settings(t, 0)
	n := h.node(t, name, "eu-central", "shared", 1500)
	inst, err := store.New(h.db).UpsertSharedInstance(ctx, store.UpsertSharedInstanceParams{NodeID: n.ID, PgVersion: 18, Port: 5432})
	if err != nil {
		t.Fatal(err)
	}
	since := h.s.cfg.Now().Add(-25 * time.Hour)
	if err := store.New(h.db).SetNodeEmptySince(ctx, store.SetNodeEmptySinceParams{ID: n.ID, EmptySince: &since}); err != nil {
		t.Fatal(err)
	}
	return n, inst
}

// placeProject records a project on inst, as a placement that chose the
// node before the sweep took it out would.
func (h *harness) placeProject(t *testing.T, inst store.Instance) store.Project {
	t.Helper()
	ctx := context.Background()
	var org uuid.UUID
	if err := h.db.QueryRow(ctx, `INSERT INTO organizations (name, slug, plan_id)
		SELECT 'Org', 'org-' || substr(md5(random()::text), 1, 8), id FROM quota_plans ORDER BY created_at LIMIT 1 RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	name := "p_" + uuid.NewString()[:8]
	p, err := store.New(h.db).InsertProject(ctx, store.InsertProjectParams{
		Region: "eu-central", ID: uuid.New(), OrgID: org, Name: name, Slug: name[2:], DbName: name, OwnerRole: name + "_owner",
		ScramVerifier: "x", Tier: "shared", InstanceID: inst.ID, Settings: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (h *harness) getNode(t *testing.T, id uuid.UUID) store.Node {
	t.Helper()
	n, err := store.New(h.db).GetNode(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The race the sweep had: occupancy read once, then the server deleted. A
// project placed on the node between the sweep's read and the delete lost
// its database. Now the first sweep only takes the node out of placement;
// a project that lands there anyway (placed by something that chose the
// node just before) is counted by the next, which puts the node back
// instead of deleting it.
func TestEmptySweepPutsBackANodeSomethingWasPlacedOn(t *testing.T) {
	h := newHarness(t, 1000)
	ctx := context.Background()
	n, inst := h.emptyNode(t, "empty")

	if err := h.s.sweepEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.prov.deletedServers(); len(got) != 0 {
		t.Fatalf("the first sweep deleted %v; it should only take the node out of placement", got)
	}
	if got := h.getNode(t, n.ID); got.Lifecycle != NodeRetiring || got.RetiringAt == nil {
		t.Fatalf("the node is %s, want retiring", got.Lifecycle)
	}
	// Placement no longer offers it.
	if _, err := store.New(h.db).PickSharedInstance(ctx, store.PickSharedInstanceParams{OrgID: &uuid.Nil, PgVersion: 18, Region: "eu-central"}); err == nil {
		t.Error("a retiring node's cluster is still offered to new projects")
	}

	p := h.placeProject(t, inst) // between the sweep's read and the delete
	h.advance(h.s.cfg.RetireGrace)
	if err := h.s.sweepEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.prov.deletedServers(); len(got) != 0 {
		t.Fatalf("deleted %v with project %s on it", got, p.Name)
	}
	got := h.getNode(t, n.ID)
	if got.Lifecycle != "active" || got.Status == "removed" || got.EmptySince != nil || got.RetiringAt != nil {
		t.Errorf("the node is %s/%s (empty since %v, retiring at %v), want back in service", got.Lifecycle, got.Status, got.EmptySince, got.RetiringAt)
	}
	if i, err := store.New(h.db).GetInstance(ctx, inst.ID); err != nil || i.DeletedAt != nil {
		t.Errorf("the node's cluster was retired under its project: %v %v", err, i.DeletedAt)
	}
}

// A node that stays empty is deleted on the sweep after the grace period:
// its clusters retired and nothing placeable while the server is deleted,
// then the node removed. A failed server delete is retried by the next.
func TestEmptySweepDeletesTheServerLast(t *testing.T) {
	h := newHarness(t, 1000)
	ctx := context.Background()
	n, inst := h.emptyNode(t, "empty")
	if err := h.s.sweepEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	// Within the grace period nothing happens.
	h.advance(h.s.cfg.RetireGrace / 2)
	if err := h.s.sweepEmpty(ctx); err != nil || len(h.prov.deletedServers()) != 0 {
		t.Fatalf("within the grace period: %v, deleted %v", err, h.prov.deletedServers())
	}

	h.advance(h.s.cfg.RetireGrace)
	h.prov.failDeletes(errors.New("the provider's API is down"))
	h.prov.onDelete = func() {
		// While the server is deleted, the node's cluster is retired and
		// can't be chosen.
		if i, err := store.New(h.db).GetInstance(ctx, inst.ID); err != nil || i.DeletedAt == nil {
			t.Errorf("the server is deleted before its cluster is retired: %v", err)
		}
		if _, err := store.New(h.db).PickSharedInstance(ctx, store.PickSharedInstanceParams{OrgID: &uuid.Nil, PgVersion: 18, Region: "eu-central"}); err == nil {
			t.Error("a cluster is offered while its server is deleted")
		}
	}
	if err := h.s.sweepEmpty(ctx); err == nil {
		t.Fatal("a failed server delete wasn't reported")
	}
	if got := h.getNode(t, n.ID); got.Lifecycle != NodeRetiring || got.Status == "removed" {
		t.Fatalf("after a failed server delete the node is %s/%s, want still retiring", got.Lifecycle, got.Status)
	}

	h.prov.failDeletes(nil)
	if err := h.s.sweepEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.prov.deletedServers(); len(got) != 2 || got[1] != *n.ProviderServerID {
		t.Fatalf("server deletes: %v, want %s tried twice", got, *n.ProviderServerID)
	}
	if got := h.getNode(t, n.ID); got.Status != "removed" {
		t.Errorf("the node is %s after its server was deleted, want removed", got.Status)
	}
}

// A node marked keep while it was retiring goes back into service.
func TestEmptySweepKeepsANodeMarkedKeep(t *testing.T) {
	h := newHarness(t, 1000)
	ctx := context.Background()
	n, _ := h.emptyNode(t, "empty")
	if err := h.s.sweepEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	keep := true
	if _, err := store.New(h.db).SetNodeCost(ctx, store.SetNodeCostParams{ID: n.ID, MonthlyCostMinor: ptr(int64(1500)), CostCurrency: "EUR", Keep: &keep}); err != nil {
		t.Fatal(err)
	}
	h.advance(h.s.cfg.RetireGrace)
	if err := h.s.sweepEmpty(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.prov.deletedServers()) != 0 || h.getNode(t, n.ID).Lifecycle != "active" {
		t.Errorf("a kept node: deleted %v, lifecycle %s", h.prov.deletedServers(), h.getNode(t, n.ID).Lifecycle)
	}
}

func ptr[T any](v T) *T { return &v }
