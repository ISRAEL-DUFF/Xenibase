package integration

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/billing"
	"github.com/israel-duff/xenibase/internal/cloud"
	"github.com/israel-duff/xenibase/internal/costs"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// fillNode makes a node look 60% full of a 100 GB disk, growing 2 GB a day
// for the last week: 88% in 14 days.
func fillNode(t *testing.T, e *testenv.Env, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	n, err := store.New(e.DB).GetNodeByName(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	const gb = 1 << 30
	if _, err := e.DB.Exec(ctx, `UPDATE nodes SET capacity = jsonb_build_object('cpus', 4, 'mem_total_bytes', $2::bigint,
		'disk_total_bytes', $3::bigint, 'disk_free_bytes', $4::bigint) WHERE id = $1`, n.ID, int64(8*gb), int64(100*gb), int64(40*gb)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	for h := 7 * 24; h >= 0; h -= 6 {
		used := 60*gb - float64(h)/24*2*gb
		if _, err := e.DB.Exec(ctx, `INSERT INTO metric_points (scope, scope_id, metric, ts, resolution, value)
			VALUES ('node', $1, 'disk_used_bytes', $2, '1h', $3) ON CONFLICT DO NOTHING`, n.ID, now.Add(-time.Duration(h)*time.Hour), used); err != nil {
			t.Fatal(err)
		}
	}
	return n.ID
}

// TestCapacityProvisionsAndJoins is M24's capacity done-when: filling a
// test region triggers a proposal; over the budget it waits, within it it
// provisions a server whose agent joins by itself with the token in its
// cloud-init, gets a shared cluster, and takes new projects. Drained, the
// node empties and its server is deleted.
func TestCapacityProvisionsAndJoins(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	ctx := context.Background()
	before := e.CreateProject("Before")
	fillNode(t, e, "test")

	settings := func(budget int64) {
		t.Helper()
		st := gen.CapacitySettings{AutoApply: true, MonthlyBudgetMinor: budget, BudgetCurrency: "EUR", AutoRebalance: false, RebalanceSpread: 0.15, DeleteEmptyAfterHours: 24,
			Shared:    gen.TierSettings{Enabled: true, DiskThreshold: ptr(float32(0.7)), HorizonDays: ptr(14), ServerType: ptr("cpx31"), ClusterMemoryMb: ptr(512)},
			Dedicated: gen.TierSettings{Enabled: false}}
		if code := e.Do("PUT", "/api/v1/admin/capacity/settings", st, nil); code != http.StatusOK {
			t.Fatalf("capacity settings: %d", code)
		}
	}

	// Over the budget (€10 a month, a cpx31 is €15.11): it waits.
	settings(1000)
	var made gen.CapacityProposalList
	if code := e.Do("POST", "/api/v1/admin/capacity/evaluate", nil, &made); code != http.StatusOK || len(made.Items) != 1 {
		t.Fatalf("evaluate: %d %+v", code, made)
	}
	p := made.Items[0]
	if p.Status != gen.CapacityProposalStatusPending || p.Auto || p.ServerType != "cpx31" || p.MonthlyCostMinor != 1511 || !strings.Contains(p.Reason, "over the budget") {
		t.Fatalf("proposal over budget: %+v", p)
	}
	if code := e.Do("POST", "/api/v1/admin/capacity/evaluate", nil, &made); code != http.StatusOK || len(made.Items) != 0 {
		t.Errorf("a second proposal while one is open: %+v", made)
	}
	// The admin is told.
	if err := e.Alerts.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	if firing, _, err := e.Alerts.Firing(ctx); err != nil || firing == 0 {
		t.Errorf("no alert for a proposal waiting: %d %v", firing, err)
	}
	if code := e.Do("POST", "/api/v1/admin/capacity/proposals/"+p.Id.String()+"/reject", nil, nil); code != http.StatusOK {
		t.Fatalf("reject: %d", code)
	}
	if len(e.Hetzner.Created()) != 0 {
		t.Fatal("a server was created over the budget")
	}

	// Within the budget (€100): provisioned by itself.
	settings(10000)
	if code := e.Do("POST", "/api/v1/admin/capacity/evaluate", nil, &made); code != http.StatusOK || len(made.Items) != 1 {
		t.Fatalf("evaluate: %d %+v", code, made)
	}
	p = made.Items[0]
	if !p.Auto || p.Status != gen.CapacityProposalStatusProvisioning || p.OperationId == nil {
		t.Fatalf("proposal within budget: %+v", p)
	}
	var srv cloud.FakeServer
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if live := e.Hetzner.Live(); len(live) == 1 {
			srv = live[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no server was created")
		}
	}
	if srv.Type != "cpx31" || srv.Location != "fsn1" || srv.Labels["xenibase-region"] != "eu-central" || !strings.HasPrefix(srv.UserData, "#cloud-config") {
		t.Fatalf("server: %+v", srv)
	}
	// V3.1 §2.3: the region's spread placement group, created on first use.
	g, ok := e.Hetzner.PlacementGroups()["xenibase-eu-central"]
	if !ok || srv.PlacementGroup != g.ID || len(g.Servers) != 1 {
		t.Fatalf("placement group: %+v, server's %d", e.Hetzner.PlacementGroups(), srv.PlacementGroup)
	}
	// In this environment the "server" is the agent-test-2 container: that
	// is its private address.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		n, err := store.New(e.DB).GetNodeByName(ctx, srv.Name)
		if err == nil && n.ProviderServerID != nil {
			if _, err := e.DB.Exec(ctx, `UPDATE nodes SET private_addr = 'agent-test-2' WHERE id = $1`, n.ID); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the node didn't record its server")
		}
	}
	// The "server" boots: its cloud-init runs the agent with the token.
	token := cloud.TokenFromCloudInit(srv.UserData)
	if !strings.HasPrefix(token, "xbreg_") {
		t.Fatalf("token in cloud-init: %q", token)
	}
	e.StartSecondAgent(srv.Name, token)
	op := e.WaitOperation(*p.OperationId)
	t.Logf("provision log:\n%s", testenv.FormatLog(op))
	if op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("provision: %s %s", op.Status, deref(op.Error))
	}
	var capa gen.Capacity
	if code := e.Do("GET", "/api/v1/admin/capacity", nil, &capa); code != http.StatusOK {
		t.Fatalf("capacity: %d", code)
	}
	var node gen.Node
	for _, n := range capa.Nodes {
		if n.Name == srv.Name {
			node = n
		}
	}
	if node.Lifecycle == nil || *node.Lifecycle != gen.NodeLifecycleActive || node.PrivateAddr != "agent-test-2" || node.MonthlyCostMinor == nil || *node.MonthlyCostMinor != 1511 ||
		node.Provider == nil || *node.Provider != "hetzner" || node.PlacementGroup == nil || *node.PlacementGroup != strconv.FormatInt(g.ID, 10) {
		t.Fatalf("new node: %+v", node)
	}
	if capa.Proposals[0].Status != gen.CapacityProposalStatusDone || capa.BudgetUsedMinor != 1511 {
		t.Errorf("after provisioning: %+v used %d", capa.Proposals[0], capa.BudgetUsedMinor)
	}

	// It takes the next project.
	after := e.CreateProject("After")
	var got gen.Project
	e.Do("GET", "/api/v1/projects/"+after.Project.Id.String(), nil, &got)
	if got.Instance == nil || got.Instance.NodeName != srv.Name {
		t.Fatalf("new project placed on %+v, want %s", got.Instance, srv.Name)
	}
	conn := e.MustConnect(after.Connection.PooledUrl)
	if _, err := conn.Exec(ctx, `CREATE TABLE t (id int); INSERT INTO t VALUES (1), (2)`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)

	// The rebalancer evens the two out: the new node is nearly empty, so
	// it proposes moving Before (10 GB) there. The admin rejects the batch.
	const gb = 1 << 30
	if _, err := e.DB.Exec(ctx, `UPDATE nodes SET capacity = capacity || jsonb_build_object('disk_total_bytes', $2::bigint, 'disk_free_bytes', $3::bigint) WHERE id = $1`,
		node.Id, int64(100*gb), int64(90*gb)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(ctx, `INSERT INTO metric_points (scope, scope_id, metric, ts, resolution, value) VALUES ('project', $1, 'size_bytes', now(), '1m', $2)`,
		before.Project.Id, float64(10*gb)); err != nil {
		t.Fatal(err)
	}
	var plan gen.RebalancePlan
	if code := e.Do("POST", "/api/v1/admin/capacity/rebalance", nil, &plan); code != http.StatusOK || plan.Moves < 1 || plan.Batch == nil {
		t.Fatalf("rebalance: %d %+v", code, plan)
	}
	e.Do("GET", "/api/v1/admin/capacity", nil, &capa)
	found := false
	for _, m := range capa.Moves {
		if m.ProjectId == before.Project.Id && m.Kind == gen.Rebalance && m.Status == gen.RebalanceMoveStatusProposed && m.ToName != nil && *m.ToName == srv.Name {
			found = true
		}
	}
	if !found {
		t.Fatalf("rebalance moves: %+v", capa.Moves)
	}
	if code := e.Do("POST", "/api/v1/admin/capacity/batches/"+plan.Batch.String(), gen.BatchDecision{Approve: false}, &plan); code != http.StatusOK || plan.Moves < 1 {
		t.Fatalf("reject batch: %d %+v", code, plan)
	}

	// Drained, its project moves back, nothing new lands there, and once
	// empty for a day its server is deleted.
	var dr gen.DrainResult
	if code := e.Do("POST", "/api/v1/nodes/"+node.Id.String()+"/drain", nil, &dr); code != http.StatusOK || dr.Moves != 1 || *dr.Node.Lifecycle != gen.NodeLifecycleDraining {
		t.Fatalf("drain: %d %+v", code, dr)
	}
	third := e.CreateProject("Third")
	e.Do("GET", "/api/v1/projects/"+third.Project.Id.String(), nil, &got)
	if got.Instance == nil || got.Instance.NodeName == srv.Name {
		t.Errorf("a project was placed on a draining node")
	}
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		if err := e.Capacity.Step(ctx); err != nil {
			t.Logf("step: %v", err)
		}
		e.Do("GET", "/api/v1/projects/"+after.Project.Id.String(), nil, &got)
		if got.Status == gen.ProjectStatusActive && got.Instance != nil && got.Instance.NodeName == "test" {
			break
		}
		if time.Now().After(deadline) {
			e.Do("GET", "/api/v1/admin/capacity", nil, &capa)
			t.Fatalf("the drain didn't move the project: %+v %+v", got.Instance, capa.Moves)
		}
	}
	conn = e.MustConnect(after.Connection.PooledUrl)
	var rows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("after the drain: %d rows, %v", rows, err)
	}
	_ = conn.Close(ctx)
	_ = e.Capacity.Step(ctx) // the move's done
	// The move's retired copy is kept 48 hours; then the node is empty.
	if _, err := e.DB.Exec(ctx, `UPDATE retired_databases SET dropped_at = now() WHERE instance_id IN (SELECT id FROM instances WHERE node_id = $1)`, node.Id); err != nil {
		t.Fatal(err)
	}
	if err := e.Capacity.Step(ctx); err != nil {
		t.Fatal(err)
	}
	n, _ := store.New(e.DB).GetNode(ctx, node.Id)
	if n.EmptySince == nil {
		t.Fatalf("the drained node isn't marked empty")
	}
	if len(e.Hetzner.Live()) != 1 {
		t.Fatal("the server was deleted before a day")
	}
	if _, err := e.DB.Exec(ctx, `UPDATE nodes SET empty_since = now() - interval '25 hours' WHERE id = $1`, node.Id); err != nil {
		t.Fatal(err)
	}
	// The first sweep takes it out of placement; the server goes once the
	// grace period has passed with nothing placed there.
	if err := e.Capacity.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ = store.New(e.DB).GetNode(ctx, node.Id); n.Lifecycle != "retiring" || len(e.Hetzner.Live()) != 1 {
		t.Fatalf("after a day empty the node is %s with %d server(s), want retiring with its server", n.Lifecycle, len(e.Hetzner.Live()))
	}
	if _, err := e.DB.Exec(ctx, `UPDATE nodes SET retiring_at = now() - interval '1 hour' WHERE id = $1`, node.Id); err != nil {
		t.Fatal(err)
	}
	if err := e.Capacity.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.Hetzner.Live()) != 0 {
		t.Fatal("the empty server wasn't deleted")
	}
	if n, _ = store.New(e.DB).GetNode(ctx, node.Id); n.Status != "removed" {
		t.Errorf("node after deletion: %s", n.Status)
	}
}

// TestCostAttributionMatchesManual is M24's cost done-when: a day's node,
// backup, egress and floating-IP costs are attributed to organisations as
// the rules say, and each one's margin matches a calculation by hand.
func TestCostAttributionMatchesManual(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	if _, err := e.Billing.ChangePlan(ctx, e.OrgID, billing.PlanRequest{Plan: billing.PlanPro}); err != nil {
		t.Fatal(err)
	}
	// Before anything is attributed: empty lists, not nulls (the page maps them).
	if code, body := e.GetText("/api/v1/admin/costs", nil, true); code != http.StatusOK || !strings.Contains(body, `"categories":[]`) {
		t.Errorf("empty month: %d %s", code, body)
	}
	a := e.CreateProject("A")
	freeOrg := e.CreateOrg("Hobby")
	b := e.CreateProjectIn("B", freeOrg)

	// The shared node costs €40.00 a month; €1 is ₦1,700.
	var node gen.Node
	nodeID := mustNodeID(t, e, "test")
	if code := e.Do("PUT", "/api/v1/nodes/"+nodeID.String()+"/cost", gen.NodeCost{MonthlyCostMinor: ptr(int64(4000)), Currency: "EUR"}, &node); code != http.StatusOK {
		t.Fatalf("node cost: %d", code)
	}
	// It has been in service for a while (attribution counts nodes that
	// existed on the day).
	if _, err := e.DB.Exec(ctx, `UPDATE nodes SET capacity = '{"cpus": 4}', created_at = now() - interval '30 days' WHERE id = $1`, nodeID); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	if code := e.Do("POST", "/api/v1/admin/fx-rates", gen.FXRateInput{Currency: "EUR", NgnPerUnit: 1700, EffectiveAt: ptr(day.AddDate(0, 0, -10))}, nil); code != http.StatusCreated {
		t.Fatalf("fx rate: %d", code)
	}

	// A day's usage: A has 3× B's storage and connection-hours, a backup
	// and some transfer.
	use := func(org, project uuid.UUID, metric string, qty float64) {
		t.Helper()
		if _, err := e.DB.Exec(ctx, `INSERT INTO usage_records (org_id, project_id, metric, granularity, period_start, quantity, plan_id)
			VALUES ($1, $2, $3, 'day', $4, $5, (SELECT plan_id FROM organizations WHERE id = $1))`, org, project, metric, day.Add(time.Hour), qty); err != nil {
			t.Fatal(err)
		}
	}
	conns := func(project uuid.UUID, hours float64) {
		t.Helper()
		if _, err := e.DB.Exec(ctx, `INSERT INTO metric_points (scope, scope_id, metric, ts, resolution, value) VALUES ('project', $1, 'connections_active', $2, '1h', $3)`,
			project, day.Add(2*time.Hour), hours); err != nil {
			t.Fatal(err)
		}
	}
	use(e.OrgID, a.Project.Id, "shared_storage_gb_hours", 72)
	use(freeOrg, b.Project.Id, "shared_storage_gb_hours", 24)
	use(e.OrgID, a.Project.Id, "backup_storage_gb_hours", 240)
	use(e.OrgID, a.Project.Id, "pooler_transfer_gb", 10)
	conns(a.Project.Id, 30)
	conns(b.Project.Id, 10)

	if code := e.Do("POST", "/api/v1/admin/costs/attribute", gen.AttributeRequest{From: date(day), To: date(day)}, nil); code != http.StatusOK {
		t.Fatalf("attribute: %d", code)
	}
	var m gen.Margins
	if code := e.Do("GET", "/api/v1/admin/costs?month="+day.Format("2006-01"), nil, &m); code != http.StatusOK {
		t.Fatalf("costs: %d", code)
	}

	// By hand, in euro cents, then kobo at 1,700.
	dm := float64(time.Date(day.Year(), day.Month()+1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1).Day())
	daily := 4000 / dm
	sharedA, sharedB := daily*0.75, daily*0.25 // both shares are 3:1
	backupA := costs.DefaultSettings().ObjectStorageGBMonth * 240 / (24 * dm)
	egressA := 0.1 * 10
	floating := 357 / dm
	kobo := func(cents float64) int64 { return int64(math.Round(cents * 1700)) }
	orgs := map[uuid.UUID]gen.OrgMargin{}
	for _, o := range m.Orgs {
		orgs[o.OrgId] = o
	}
	oa, ob := orgs[e.OrgID], orgs[freeOrg]
	if d := oa.CostMinor - kobo(sharedA+backupA+egressA); d < -1 || d > 1 {
		t.Errorf("A's cost: %d, by hand %d", oa.CostMinor, kobo(sharedA+backupA+egressA))
	}
	if d := ob.CostMinor - kobo(sharedB); d < -1 || d > 1 {
		t.Errorf("B's cost: %d, by hand %d", ob.CostMinor, kobo(sharedB))
	}
	rated, err := e.Billing.Rate(ctx, e.OrgID, day)
	if err != nil {
		t.Fatal(err)
	}
	wantRevenue := rated.Subtotal - rated.NextFee + rated.MonthFee
	if oa.RevenueMinor != wantRevenue || oa.MarginMinor != wantRevenue-oa.CostMinor || oa.Plan != "pro" {
		t.Errorf("A: %+v, want revenue %d", oa, wantRevenue)
	}
	if ob.RevenueMinor != 0 || ob.Plan != "free" || m.FreeTierCostMinor != ob.CostMinor {
		t.Errorf("B: %+v, free tier %d", ob, m.FreeTierCostMinor)
	}
	if d := m.UnallocatedMinor - kobo(floating); d < -1 || d > 1 {
		t.Errorf("unallocated: %d, by hand %d", m.UnallocatedMinor, kobo(floating))
	}
	if d := m.CostMinor - (oa.CostMinor + ob.CostMinor + m.UnallocatedMinor); d < -2 || d > 2 {
		t.Errorf("total cost %d isn't its parts", m.CostMinor)
	}
	if m.Days != 1 || m.Rates["EUR"] != 1700 {
		t.Errorf("days %d rates %+v", m.Days, m.Rates)
	}

	// The naira weakens: today's rate makes the same costs dearer, and the
	// erosion shows.
	if code := e.Do("POST", "/api/v1/admin/fx-rates", gen.FXRateInput{Currency: "EUR", NgnPerUnit: 1870}, nil); code != http.StatusCreated {
		t.Fatalf("fx rate: %d", code)
	}
	var m2 gen.Margins
	e.Do("GET", "/api/v1/admin/costs?month="+day.Format("2006-01"), nil, &m2)
	if m2.CostMinor <= m.CostMinor || m2.FxErosionMinor <= 0 || m2.CostBookedMinor != m.CostMinor {
		t.Errorf("FX view: cost %d → %d, booked %d, erosion %d", m.CostMinor, m2.CostMinor, m2.CostBookedMinor, m2.FxErosionMinor)
	}
	code, csv := e.GetText("/api/v1/admin/costs?format=csv&month="+day.Format("2006-01"), nil, true)
	if code != http.StatusOK || !strings.Contains(csv, "gross margin") || !strings.Contains(csv, "Hobby") {
		t.Errorf("CSV: %d %.200s", code, csv)
	}
}

func mustNodeID(t *testing.T, e *testenv.Env, name string) uuid.UUID {
	t.Helper()
	n, err := store.New(e.DB).GetNodeByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return n.ID
}

func date(t time.Time) openapi_types.Date { return openapi_types.Date{Time: t} }
