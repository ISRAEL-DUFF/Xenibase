package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/cloud"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// resize asks for a new size and waits for what it queued.
func resize(t *testing.T, e *testenv.Env, project uuid.UUID, req gen.InstanceUpdate) (gen.InstanceUpdated, gen.Operation) {
	t.Helper()
	var upd gen.InstanceUpdated
	if code := e.Do("PATCH", "/api/v1/projects/"+project.String()+"/instance", req, &upd); code != http.StatusOK || upd.Operation == nil {
		t.Fatalf("resize %+v: %d %+v", req, code, upd)
	}
	op := e.WaitOperation(upd.Operation.Id)
	t.Logf("%s:\n%s", op.Kind, testenv.FormatLog(op))
	if op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("%s: %s %s", op.Kind, op.Status, deref(op.Error))
	}
	return upd, op
}

// limits are a member container's CPU and memory limits.
func limits(t *testing.T, member uuid.UUID) string {
	t.Helper()
	got, err := dockerInspect(t, "xenibase-"+member.String(), "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}")
	if err != nil {
		t.Fatalf("inspect %s: %v", member, err)
	}
	return got
}

// TestDedicatedResize is V4.1-M4's done-when for resizing (V4.1 §5.5): a
// dedicated project under a live writer goes from 1 to 2 vCPU with a short
// pause and no lost commit, and the next hour's usage carries the new size;
// its disk grows and the warning follows; a size its node can't hold moves
// it to a node that can.
func TestDedicatedResize(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	e.ConfigureBackups()
	e.SetNodeRole("test", "dedicated")
	ns := threeNodes(t, e, gen.CreateNodeRequestRoleDedicated)
	ctx := context.Background()
	tier, profile, vol := gen.ProjectTierDedicated, "small", 5
	nodeID := ns[0].Id
	var c gen.ProjectCredentials
	if code := e.Do("POST", "/api/v1/projects", gen.CreateProjectRequest{Name: "Growing", Tier: &tier, Profile: &profile, VolumeGb: &vol, NodeId: &nodeID}, &c); code != http.StatusAccepted {
		t.Fatalf("create: %d", code)
	}
	if op := e.WaitOperation(c.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("create: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	pid := c.Project.Id
	p, err := store.New(e.DB).GetProject(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}

	// ---- 1 → 2 vCPU in place, under writes ------------------------------------
	w := startWriter(t, e, c.Connection.PooledUrl)
	time.Sleep(time.Second)
	cpus, mem := float32(2), 2048
	upd, op := resize(t, e, pid, gen.InstanceUpdate{Cpus: &cpus, MemoryMb: &mem})
	if upd.Plan == nil || upd.Plan.MoveTo != nil || !upd.Plan.Restart || op.Kind != dedicated.KindResize {
		t.Fatalf("plan %+v, %s", upd.Plan, op.Kind)
	}
	time.Sleep(time.Second)
	w.Stop()
	if w.paused.Load() >= 10_000 {
		t.Errorf("clients waited %d ms during the resize", w.paused.Load())
	}
	t.Logf("longest gap between commits: %d ms; %d commits, %d client errors", w.paused.Load(), w.acked.Load(), w.errs.Load())
	conn := e.MustConnect(c.Connection.PooledUrl)
	var rows int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM ledger`).Scan(&rows); err != nil || rows < w.acked.Load() {
		t.Fatalf("ledger after the resize: %d rows, %d acknowledged %v", rows, w.acked.Load(), err)
	}
	_ = conn.Close(ctx)
	if got := limits(t, p.InstanceID); got != "2000000000 2147483648" {
		t.Fatalf("container limits after the resize: %s", got)
	}
	// What the usage recorder reads for this hour: the new size.
	hour := time.Now().UTC().Truncate(time.Hour)
	ded, err := store.New(e.DB).HourlyDedicated(ctx, store.HourlyDedicatedParams{FromTs: hour, LastHour: hour})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range ded {
		if r.ProjectID == pid {
			found = r.Cpus == 2 && r.MemMb == 2048
		}
	}
	if !found {
		t.Fatalf("this hour's dedicated usage: %+v", ded)
	}

	// ---- The disk grows, the warning follows; it never shrinks -----------------
	disk := 10
	if _, op := resize(t, e, pid, gen.InstanceUpdate{DiskGb: &disk}); !strings.Contains(testenv.FormatLog(op), "disk 5 → 10 GB") {
		t.Errorf("disk log:\n%s", testenv.FormatLog(op))
	}
	var gp gen.Project
	e.Do("GET", "/api/v1/projects/"+pid.String(), nil, &gp)
	if gp.Settings.DiskWarnBytes != dedicated.DefaultDiskWarn(10) || gp.Instance == nil || gp.Instance.VolumeGb == nil || *gp.Instance.VolumeGb != 10 {
		t.Fatalf("after growing the disk: warn %d, instance %+v", gp.Settings.DiskWarnBytes, gp.Instance)
	}
	small := 5
	var apiErr gen.Error
	if code := e.Do("PATCH", "/api/v1/projects/"+pid.String()+"/instance", gen.InstanceUpdate{DiskGb: &small}, &apiErr); code != http.StatusBadRequest || !strings.Contains(apiErr.Message, "only grows") {
		t.Fatalf("shrinking the disk: %d %+v", code, apiErr)
	}

	// ---- A size the node can't hold moves the project -------------------------
	// Fill node "test" by memory: a placeholder instance (no CPUs) holding
	// all but 256 MB of what the node reports and its instances leave, so
	// 512 MB more doesn't fit there. Memory, not CPUs, so the test needs
	// no particular number of cores: the nodes share this host, and Docker
	// refuses a CPU limit above the host's cores but not a memory limit.
	var freeMB int64
	if err := e.DB.QueryRow(ctx, `SELECT ((capacity->>'mem_total_bytes')::bigint >> 20)
		  - (SELECT coalesce(sum(mem_limit_mb), 0) FROM instances WHERE node_id = $1 AND deleted_at IS NULL)
		FROM nodes WHERE id = $1`, ns[0].Id).Scan(&freeMB); err != nil {
		t.Fatalf("node test's free memory: %v", err)
	}
	filler := uuid.New()
	if _, err := e.DB.Exec(ctx, `INSERT INTO instances (id, node_id, kind, pg_version, port, status, cpu_limit, mem_limit_mb, volume_gb)
		VALUES ($1, $2, 'dedicated', 17, 0, 'stopped', 0, $3, 1)`, filler, ns[0].Id, max(freeMB-256, 0)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.DB.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, filler) })
	bigger, yes := 2560, true
	var dry gen.InstanceUpdated
	if code := e.Do("PATCH", "/api/v1/projects/"+pid.String()+"/instance", gen.InstanceUpdate{MemoryMb: &bigger, DryRun: &yes}, &dry); code != http.StatusOK ||
		dry.Plan == nil || dry.Plan.MoveTo == nil || dry.Operation != nil {
		t.Fatalf("dry run that doesn't fit: %d %+v", code, dry)
	}
	moveTo := *dry.Plan.MoveTo
	_, op = resize(t, e, pid, gen.InstanceUpdate{MemoryMb: &bigger})
	if op.Kind != dedicated.KindMove {
		t.Fatalf("a resize that doesn't fit queued %s", op.Kind)
	}
	e.Do("GET", "/api/v1/projects/"+pid.String(), nil, &gp)
	if gp.Instance == nil || gp.Instance.NodeName != moveTo || gp.Instance.Cpus == nil || *gp.Instance.Cpus != 2 ||
		gp.Instance.MemoryMb == nil || *gp.Instance.MemoryMb != 2560 ||
		gp.Instance.VolumeGb == nil || *gp.Instance.VolumeGb != 10 || gp.Settings.DiskWarnBytes != dedicated.DefaultDiskWarn(10) {
		t.Fatalf("after the moving resize: %+v warn %d", gp.Instance, gp.Settings.DiskWarnBytes)
	}
	if got := limits(t, gp.Instance.Id); got != "2000000000 2684354560" {
		t.Fatalf("container limits after the move: %s", got)
	}
	conn = e.MustConnect(c.Connection.PooledUrl)
	defer conn.Close(ctx)
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM ledger`).Scan(&rows); err != nil || rows < w.acked.Load() {
		t.Fatalf("ledger after the move: %d rows, %d acknowledged %v", rows, w.acked.Load(), err)
	}
}

// TestDedicatedResizeHA: with HA the standby is resized first, the project
// switches over to it, then the old primary is resized; clients see only
// the switchover's pause.
func TestDedicatedResizeHA(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	c, ns := haProject(t, e)
	ctx := context.Background()
	standbyNode := ns[1].Id
	op, err := e.Dedicated.EnableHA(ctx, dedicated.HAParams{ProjectID: c.Project.Id, NodeID: &standbyNode})
	if err != nil {
		t.Fatal(err)
	}
	if g := e.WaitOperation(op.ID); g.Status != gen.OperationStatusSucceeded {
		t.Fatalf("enable HA: %s %s\n%s", g.Status, deref(g.Error), testenv.FormatLog(g))
	}
	before := haStatus(t, e, c.Project.Id)
	oldLeader, _ := leaderOf(before)

	w := startWriter(t, e, c.Connection.PooledUrl)
	time.Sleep(time.Second)
	// 3 GB: max_connections grows from 100 to 150, a change in Patroni's
	// cluster configuration that every member takes on its restart.
	cpus, mem := float32(2), 3072
	resize(t, e, c.Project.Id, gen.InstanceUpdate{Cpus: &cpus, MemoryMb: &mem})
	time.Sleep(time.Second)
	w.Stop()
	t.Logf("longest gap between commits: %d ms; %d commits, %d client errors", w.paused.Load(), w.acked.Load(), w.errs.Load())
	if w.paused.Load() >= 10_000 {
		t.Errorf("clients waited %d ms during the HA resize", w.paused.Load())
	}
	after := haStatus(t, e, c.Project.Id)
	newLeader, ok := leaderOf(after)
	if !ok || newLeader.NodeId == oldLeader.NodeId {
		t.Fatalf("leader before %+v, after %+v", oldLeader, newLeader)
	}
	p, err := store.New(e.DB).GetProject(ctx, c.Project.Id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members(t, e, p.InstanceID) {
		if got := limits(t, m.ID); got != "2000000000 3221225472" {
			t.Errorf("member on %s: limits %s", m.NodeName, got)
		}
		if got := memberSQL(t, m.ID, "postgres", "SHOW max_connections"); got != "150" {
			t.Errorf("member on %s: max_connections %s", m.NodeName, got)
		}
	}
	conn := e.MustConnect(c.Connection.PooledUrl)
	defer conn.Close(ctx)
	var rows int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM ledger`).Scan(&rows); err != nil || rows < w.acked.Load() {
		t.Fatalf("ledger after the HA resize: %d rows, %d acknowledged %v", rows, w.acked.Load(), err)
	}
}

// TestDedicatedHostOnDemand is the hosts-on-demand done-when (V4.1 §5.5):
// with the fake Hetzner and no dedicated node, a dedicated create over the
// budget is refused with capacity_pending_approval and an open proposal;
// within it, a node is provisioned and the project lands on it.
func TestDedicatedHostOnDemand(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	e.ConfigureBackups()
	e.SetNodeRole("test", "shared")
	ctx := context.Background()
	settings := func(budget int64) {
		t.Helper()
		st := gen.CapacitySettings{AutoApply: true, MonthlyBudgetMinor: budget, BudgetCurrency: "EUR", RebalanceSpread: 0.15, DeleteEmptyAfterHours: 24,
			Shared:    gen.TierSettings{Enabled: false, DiskThreshold: ptr(float32(0.7)), HorizonDays: ptr(14), ClusterMemoryMb: ptr(512)},
			Dedicated: gen.TierSettings{Enabled: true, ServerType: ptr("cpx31")}}
		if code := e.Do("PUT", "/api/v1/admin/capacity/settings", st, nil); code != http.StatusOK {
			t.Fatalf("capacity settings: %d", code)
		}
	}
	tier, profile := gen.ProjectTierDedicated, "small"
	create := func(name string, out any) int {
		return e.Do("POST", "/api/v1/projects", gen.CreateProjectRequest{Name: name, Tier: &tier, Profile: &profile}, out)
	}

	// Over the budget: refused, and the proposal waits for the admin.
	settings(1000)
	var apiErr gen.Error
	if code := create("Waits", &apiErr); code != http.StatusConflict || apiErr.Code != "capacity_pending_approval" {
		t.Fatalf("create over the budget: %d %+v", code, apiErr)
	}
	var capa gen.Capacity
	e.Do("GET", "/api/v1/admin/capacity", nil, &capa)
	if len(capa.Proposals) != 1 || capa.Proposals[0].Status != gen.CapacityProposalStatusPending || capa.Proposals[0].Tier != "dedicated" {
		t.Fatalf("proposals over the budget: %+v", capa.Proposals)
	}
	if len(e.Hetzner.Created()) != 0 {
		t.Fatal("a server was created over the budget")
	}
	if code := e.Do("POST", "/api/v1/admin/capacity/proposals/"+capa.Proposals[0].Id.String()+"/reject", nil, nil); code != http.StatusOK {
		t.Fatalf("reject: %d", code)
	}

	// Within it: the create waits for a new node and lands there.
	settings(10000)
	var c gen.ProjectCredentials
	if code := create("Lands", &c); code != http.StatusAccepted {
		t.Fatalf("create within the budget: %d", code)
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
	// The "server" is the agent-test-2 container (see TestCapacityProvisionsAndJoins).
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
	e.StartSecondAgent(srv.Name, cloud.TokenFromCloudInit(srv.UserData))
	op := e.WaitOperation(c.Operation.Id)
	log := testenv.FormatLog(op)
	t.Logf("create:\n%s", log)
	if op.Status != gen.OperationStatusSucceeded || !strings.Contains(log, "waiting for a host") {
		t.Fatalf("create on a new host: %s %s", op.Status, deref(op.Error))
	}
	var gp gen.Project
	e.Do("GET", "/api/v1/projects/"+c.Project.Id.String(), nil, &gp)
	if gp.Instance == nil || gp.Instance.NodeName != srv.Name {
		t.Fatalf("placed on %+v, want %s", gp.Instance, srv.Name)
	}
	conn := e.MustConnect(c.Connection.PooledUrl)
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE t (id int); INSERT INTO t VALUES (1)`); err != nil {
		t.Fatal(err)
	}
}
