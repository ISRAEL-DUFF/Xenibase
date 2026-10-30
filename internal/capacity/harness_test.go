package capacity

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/cloud"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeProvider is a cloud provider whose catalog has one server type, at
// monthly euro cents a month, and which records the servers deleted.
// onDelete, when set, runs inside DeleteServer, before it returns.
type fakeProvider struct {
	monthly int64

	mu       sync.Mutex
	deleted  []string
	onDelete func()
	// deleteErr fails DeleteServer (after recording the attempt).
	deleteErr error
}

func (*fakeProvider) Name() string { return cloud.Hetzner }

func (*fakeProvider) EnsurePlacementGroup(context.Context, string, map[string]string) (cloud.PlacementGroup, error) {
	return cloud.PlacementGroup{}, nil
}

func (*fakeProvider) CreateServer(context.Context, cloud.ServerSpec) (cloud.Server, error) {
	return cloud.Server{}, cloud.ErrManual
}

func (f *fakeProvider) DeleteServer(_ context.Context, id string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, id)
	hook, err := f.onDelete, f.deleteErr
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (f *fakeProvider) failDeletes(err error) {
	f.mu.Lock()
	f.deleteErr = err
	f.mu.Unlock()
}

func (f *fakeProvider) deletedServers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (*fakeProvider) ListServers(context.Context, cloud.Filter) ([]cloud.Server, error) {
	return nil, nil
}

func (*fakeProvider) CreateVolume(context.Context, cloud.VolumeSpec) (cloud.Volume, error) {
	return cloud.Volume{}, cloud.ErrManual
}

func (*fakeProvider) AttachVolume(context.Context, string, string) error { return cloud.ErrManual }

func (*fakeProvider) AssignFloatingIP(context.Context, string, string) error { return cloud.ErrManual }

func (f *fakeProvider) PriceCatalog(context.Context) ([]cloud.ServerPrice, error) {
	return []cloud.ServerPrice{{Type: "cpx51", Location: "fsn1", CPUs: 16, MemoryGB: 32, DiskGB: 360, MonthlyMinor: f.monthly, Currency: "EUR"}}, nil
}

// harness is a capacity Service against a throwaway metadata database,
// with fakeProvider as its cloud.
type harness struct {
	s    *Service
	db   *pgxpool.Pool
	prov *fakeProvider

	mu  sync.Mutex
	now time.Time
}

func newHarness(t *testing.T, serverMonthly int64) *harness {
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
	prov := &fakeProvider{monthly: serverMonthly}
	h := &harness{db: db, prov: prov, now: time.Now()}
	h.s = New(db, ns, nil, prov, nil, Config{Region: "eu-central", Location: "fsn1", Now: func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.now
	}}, quiet)
	return h
}

// advance moves the harness's clock on.
func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

// settings saves the defaults with automatic provisioning on and a budget.
func (h *harness) settings(t *testing.T, budget int64) {
	t.Helper()
	st := DefaultSettings()
	st.AutoApply, st.MonthlyBudgetMinor, st.BudgetCurrency = true, budget, "EUR"
	if err := h.s.SetSettings(context.Background(), st); err != nil {
		t.Fatal(err)
	}
}

// region adds a region.
func (h *harness) region(t *testing.T, id string) {
	t.Helper()
	if err := store.New(h.db).EnsureRegion(context.Background(), store.EnsureRegionParams{ID: id, Name: id}); err != nil {
		t.Fatal(err)
	}
}

// node adds a healthy node in region costing monthly euro cents, with a
// provider server.
func (h *harness) node(t *testing.T, name, region, role string, monthly int64) store.Node {
	t.Helper()
	ctx := context.Background()
	q := store.New(h.db)
	h.region(t, region)
	n, err := q.UpsertNode(ctx, store.UpsertNodeParams{Name: name, PrivateAddr: "10.0.0.1", Role: role})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.SetNodeCost(ctx, store.SetNodeCostParams{ID: n.ID, MonthlyCostMinor: &monthly, CostCurrency: "EUR", Region: &region}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(ctx, `UPDATE nodes SET status = 'healthy', provider = 'hetzner', provider_server_id = $2, agent_cert_fp = 'fp' WHERE id = $1`,
		n.ID, "srv-"+name); err != nil {
		t.Fatal(err)
	}
	if n, err = q.GetNode(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	return n
}

// proposals counts the proposals by status.
func (h *harness) proposals(t *testing.T) map[string]int {
	t.Helper()
	rows, err := store.New(h.db).ListCapacityProposals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, p := range rows {
		out[p.Status]++
	}
	return out
}
