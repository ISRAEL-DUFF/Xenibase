package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/statusapi"
	"github.com/israel-duff/xenibase/internal/statuspage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// haProject is a dedicated project on node "test", with the etcd cluster
// on test, node-b and node-c, all three taking dedicated instances.
func haProject(t *testing.T, e *testenv.Env) (gen.ProjectCredentials, []gen.Node) {
	t.Helper()
	e.StartAgent()
	e.ConfigureBackups()
	e.SetNodeRole("test", "dedicated")
	ns := threeNodes(t, e, gen.CreateNodeRequestRoleDedicated)
	setupEtcd(t, e, ns)
	tier, profile, vol := gen.ProjectTierDedicated, "small", 5
	var c gen.ProjectCredentials
	nodeID := ns[0].Id
	if code := e.Do("POST", "/api/v1/projects", gen.CreateProjectRequest{Name: "Always on", Tier: &tier, Profile: &profile, VolumeGb: &vol, NodeId: &nodeID}, &c); code != http.StatusAccepted {
		t.Fatalf("create: %d", code)
	}
	if op := e.WaitOperation(c.Operation.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("create: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	return c, ns
}

// memberSQL runs a query as the instance's admin inside a member's
// container (local trust), so a standby can be read directly.
func memberSQL(t *testing.T, member uuid.UUID, db, query string) string {
	t.Helper()
	out, err := exec.Command("docker", "exec", "xenibase-"+member.String(), "psql", "-XAtq", "-h", "/var/run/postgresql",
		"-U", "xenibase_admin", "-d", db, "-c", query).CombinedOutput()
	if err != nil {
		t.Fatalf("psql on %s: %v: %s", member, err, out)
	}
	return strings.TrimSpace(string(out))
}

// memberSQLTry is memberSQL for a loop that waits for a member to be ready: a
// member that is restarting (a replica following a new primary) has no socket
// for a moment, and that is an answer to retry, not a failure.
func memberSQLTry(member uuid.UUID, db, query string) (string, error) {
	out, err := exec.Command("docker", "exec", "xenibase-"+member.String(), "psql", "-XAtq", "-h", "/var/run/postgresql",
		"-U", "xenibase_admin", "-d", db, "-c", query).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("psql on %s: %w: %s", member, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func members(t *testing.T, e *testenv.Env, instance uuid.UUID) []store.ListInstanceMembersRow {
	t.Helper()
	ms, err := store.New(e.DB).ListInstanceMembers(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestEnableAndDisableHA(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	c, ns := haProject(t, e)
	ctx := context.Background()
	p, err := store.New(e.DB).GetProject(ctx, c.Project.Id)
	if err != nil {
		t.Fatal(err)
	}

	w := startWriter(t, e, c.Connection.PooledUrl)
	time.Sleep(time.Second)
	standbyNode := ns[1].Id
	op, err := e.Dedicated.EnableHA(ctx, dedicated.HAParams{ProjectID: p.ID, NodeID: &standbyNode})
	if err != nil {
		t.Fatal(err)
	}
	g := e.WaitOperation(op.ID)
	t.Logf("enable HA:\n%s", testenv.FormatLog(g))
	if g.Status != gen.OperationStatusSucceeded {
		t.Fatalf("enable HA: %s %s", g.Status, deref(g.Error))
	}
	time.Sleep(time.Second)
	w.Stop()
	if w.paused.Load() >= 10_000 {
		t.Errorf("clients waited %d ms during the switch to Patroni", w.paused.Load())
	}
	t.Logf("longest gap between commits while HA was turned on: %d ms; %d commits, %d client errors", w.paused.Load(), w.acked.Load(), w.errs.Load())

	inst, err := store.New(e.DB).GetInstance(ctx, p.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if !inst.HaEnabled || !inst.Patroni {
		t.Fatalf("instance: ha %v patroni %v", inst.HaEnabled, inst.Patroni)
	}
	ms := members(t, e, inst.ID)
	if len(ms) != 2 {
		t.Fatalf("members: %+v", ms)
	}
	var standby uuid.UUID
	for _, m := range ms {
		switch {
		case m.ID == inst.ID && m.Role == "leader" && m.NodeID == ns[0].Id:
		case m.ID != inst.ID && m.Role == "replica" && m.NodeID == ns[1].Id:
			standby = m.ID
		default:
			t.Fatalf("member: %s on %s is %s", m.ID, m.NodeName, m.Role)
		}
	}
	// Every acknowledged commit is on the standby.
	deadline := time.Now().Add(30 * time.Second)
	for {
		n, _ := strconv.ParseInt(memberSQL(t, standby, p.DbName, `SELECT coalesce(max(n), 0) FROM ledger`), 10, 64)
		if n >= w.acked.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("standby has ledger up to %d, %d acknowledged", n, w.acked.Load())
		}
		time.Sleep(500 * time.Millisecond)
	}
	if got := memberSQL(t, standby, p.DbName, `SELECT pg_is_in_recovery()`); got != "t" {
		t.Fatalf("standby in recovery: %s", got)
	}

	// Off again: the standby goes, the primary keeps serving.
	op, err = e.Dedicated.DisableHA(ctx, p.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g := e.WaitOperation(op.ID); g.Status != gen.OperationStatusSucceeded {
		t.Fatalf("disable HA: %s %s\n%s", g.Status, deref(g.Error), testenv.FormatLog(g))
	}
	if ms := members(t, e, inst.ID); len(ms) != 1 || ms[0].ID != inst.ID {
		t.Fatalf("members after disabling: %+v", ms)
	}
	if out, err := exec.Command("docker", "inspect", "xenibase-"+standby.String()).CombinedOutput(); err == nil {
		t.Fatalf("the standby's container is still there: %s", out[:min(len(out), 200)])
	}
	conn := e.MustConnect(c.Connection.PooledUrl)
	defer conn.Close(ctx)
	var count int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM ledger`).Scan(&count); err != nil || count < w.acked.Load() {
		t.Fatalf("ledger after disabling HA: %d rows (%d acknowledged) %v", count, w.acked.Load(), err)
	}
}

// haStatus reads GET /projects/{id}/ha.
func haStatus(t *testing.T, e *testenv.Env, project uuid.UUID) gen.HAStatus {
	t.Helper()
	var st gen.HAStatus
	if code := e.Do("GET", "/api/v1/projects/"+project.String()+"/ha", nil, &st); code != http.StatusOK {
		t.Fatalf("GET ha: %d", code)
	}
	return st
}

func leaderOf(st gen.HAStatus) (gen.HAMember, bool) {
	for _, m := range st.Members {
		if m.Role == gen.HAMemberRoleLeader {
			return m, true
		}
	}
	return gen.HAMember{}, false
}

// TestHASwitchoverAndFailover: with synchronous replication, a planned
// switchover moves the primary with no lost commit; then the new primary's
// node dies (its agent, the member and its etcd member) under load, and
// writes come back on the other node through the same URL.
func TestHASwitchoverAndFailover(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	c, ns := haProject(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Dedicated.RunHAWatcher(ctx, time.Second)

	sync := true
	var op gen.Operation
	if code := e.Do("POST", "/api/v1/projects/"+c.Project.Id.String()+"/ha", gen.HAEnableRequest{NodeId: &ns[1].Id, Synchronous: &sync}, &op); code != http.StatusAccepted {
		t.Fatalf("enable HA: %d", code)
	}
	if op = e.WaitOperation(op.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("enable HA: %s %s\n%s", op.Status, deref(op.Error), testenv.FormatLog(op))
	}
	st := haStatus(t, e, c.Project.Id)
	if !st.Enabled || !st.Synchronous || len(st.Members) != 2 {
		t.Fatalf("HA status: %+v", st)
	}
	// A second enable is refused.
	if code := e.Do("POST", "/api/v1/projects/"+c.Project.Id.String()+"/ha", gen.HAEnableRequest{}, nil); code != http.StatusConflict {
		t.Fatalf("enable twice: %d", code)
	}

	// Planned switchover: test -> node-b, once Patroni reports the standby
	// streaming as the synchronous one (on a busy host that can trail the
	// enable by a few seconds, and a switchover refuses until then).
	w := startWriter(t, e, c.Connection.PooledUrl)
	time.Sleep(2 * time.Second)
	waitFor(t, 60*time.Second, "a streaming synchronous standby", func() bool {
		for _, m := range haStatus(t, e, c.Project.Id).Members {
			if m.Role == "sync_standby" && m.State != nil && *m.State == "streaming" {
				return true
			}
		}
		return false
	})
	if code := e.Do("POST", "/api/v1/projects/"+c.Project.Id.String()+"/switchover", gen.SwitchoverRequest{}, &op); code != http.StatusAccepted {
		t.Fatalf("switchover: %d", code)
	}
	if op = e.WaitOperation(op.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("switchover: %s %s\n%s", op.Status, deref(op.Error), testenv.FormatLog(op))
	}
	t.Logf("switchover:\n%s", testenv.FormatLog(op))
	time.Sleep(2 * time.Second)
	switchGap := w.paused.Load()
	st = haStatus(t, e, c.Project.Id)
	l, ok := leaderOf(st)
	if !ok || l.NodeId != ns[1].Id {
		t.Fatalf("leader after the switchover: %+v", st.Members)
	}
	if len(st.Failovers) != 1 || st.Failovers[0].Kind != gen.Switchover {
		t.Fatalf("history after the switchover: %+v", st.Failovers)
	}
	t.Logf("switchover: longest gap between commits %d ms", switchGap)

	// Both members back in place: the old primary streams again (as the
	// synchronous standby) before the new primary's node dies.
	deadline := time.Now().Add(60 * time.Second)
	for {
		ok := 0
		for _, m := range haStatus(t, e, c.Project.Id).Members {
			if m.Role == gen.HAMemberRoleLeader || (m.Role == gen.HAMemberRoleSyncStandby && m.State != nil && *m.State == "streaming") {
				ok++
			}
		}
		if ok == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("members after the switchover: %+v", haStatus(t, e, c.Project.Id).Members)
		}
		time.Sleep(time.Second)
	}

	// The new primary's node dies.
	w.paused.Store(0)
	e.KillAgent("node-b")
	killed := time.Now()
	for _, name := range []string{"xenibase-" + l.Id.String(), "xenibase-etcd-" + ns[1].Id.String()} {
		if out, err := exec.Command("docker", "kill", name).CombinedOutput(); err != nil {
			t.Fatalf("kill %s: %v %s", name, err, out)
		}
	}
	// Writes come back through the same URL.
	before := w.acked.Load()
	deadline = time.Now().Add(90 * time.Second)
	for w.acked.Load() < before+20 {
		if time.Now().After(deadline) {
			out, _ := exec.Command("docker", "logs", "--tail", "30", "xenibase-"+c.Project.Id.String()).CombinedOutput()
			for _, m := range haStatus(t, e, c.Project.Id).Members {
				if m.NodeId == ns[0].Id {
					out, _ = exec.Command("docker", "logs", "--tail", "40", "xenibase-"+m.Id.String()).CombinedOutput()
				}
			}
			t.Fatalf("no writes after the node died: %d acked before, %d now, %d errors\nsurviving member:\n%s", before, w.acked.Load(), w.errs.Load(), out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	restored := time.Since(killed)
	time.Sleep(2 * time.Second)
	w.Stop()
	t.Logf("failover: writes back after %s (longest gap between commits %d ms); %d commits, %d client errors",
		restored.Round(100*time.Millisecond), w.paused.Load(), w.acked.Load(), w.errs.Load())
	if restored > 60*time.Second {
		t.Errorf("writes came back after %s, want under 60 s", restored)
	}

	// The members' roles are what the agents last reported: the survivor's
	// can trail its promotion by a few seconds.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		st = haStatus(t, e, c.Project.Id)
		if l, ok = leaderOf(st); ok && l.NodeId == ns[0].Id {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leader after the failover: %+v", st.Members)
		}
	}
	t.Logf("leader reported %s after writes came back", time.Since(killed)-restored)
	if len(st.Failovers) != 2 || st.Failovers[0].Kind != gen.Failover || st.Failovers[0].DurationMs == nil {
		t.Fatalf("history after the failover: %+v", st.Failovers)
	}
	t.Logf("failover event: %d ms, %s -> %s", *st.Failovers[0].DurationMs, deref(st.Failovers[0].FromNode), deref(st.Failovers[0].ToNode))
	// Synchronous replication: every acknowledged commit survived.
	conn := e.MustConnect(c.Connection.PooledUrl)
	defer conn.Close(ctx)
	var count, maxN int64
	if err := conn.QueryRow(ctx, `SELECT count(*), coalesce(max(n), 0) FROM ledger`).Scan(&count, &maxN); err != nil {
		t.Fatal(err)
	}
	if maxN < w.acked.Load() || count != maxN {
		t.Fatalf("ledger after the failover: %d rows, max %d, %d acknowledged", count, maxN, w.acked.Load())
	}
}

// startSLAStatus runs xenibase-status in-process as the SLA's outside
// vantage point, probing for real, every interval.
func startSLAStatus(ctx context.Context, t *testing.T, every time.Duration) *statusapi.Client {
	t.Helper()
	dir := t.TempDir()
	cfg := `public_url = "https://status.xenibase.test"
push_secret = "` + statusSecret + `"
data = "` + filepath.Join(dir, "status.db") + `"
[[component]]
id = "dedicated"
heartbeat = true
`
	path := filepath.Join(dir, "status.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := statuspage.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := statuspage.New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	ts := httptest.NewServer(svc.Handler())
	t.Cleanup(ts.Close)
	go func() {
		tk := time.NewTicker(every)
		defer tk.Stop()
		for {
			_ = svc.SLATick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
		}
	}()
	return &statusapi.Client{URL: ts.URL, Secret: statusSecret}
}

// TestHAPrimaryNodeLoss is M19's done-when: killing an HA primary's node
// under load restores writes in under 60 seconds with the URL unchanged,
// and the availability record shows the outage.
func TestHAPrimaryNodeLoss(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	c, ns := haProject(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Dedicated.RunHAWatcher(ctx, time.Second)

	var op gen.Operation
	if code := e.Do("POST", "/api/v1/projects/"+c.Project.Id.String()+"/ha", gen.HAEnableRequest{}, &op); code != http.StatusAccepted {
		t.Fatalf("enable HA: %d", code)
	}
	if op = e.WaitOperation(op.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("enable HA: %s %s\n%s", op.Status, deref(op.Error), testenv.FormatLog(op))
	}
	st := haStatus(t, e, c.Project.Id)
	primary, ok := leaderOf(st)
	if !ok || primary.NodeId != ns[0].Id {
		t.Fatalf("members: %+v", st.Members)
	}

	// Both vantage points probe every 5 seconds (production: every minute).
	status := startSLAStatus(ctx, t, 5*time.Second)
	go e.Dedicated.RunSLA(ctx, 5*time.Second, status)
	w := startWriter(t, e, c.Connection.PooledUrl)
	// A healthy minute or so first.
	time.Sleep(70 * time.Second)

	e.KillAgent("test")
	killed := time.Now()
	for _, name := range []string{"xenibase-" + primary.Id.String(), "xenibase-etcd-" + ns[0].Id.String()} {
		if out, err := exec.Command("docker", "kill", name).CombinedOutput(); err != nil {
			t.Fatalf("kill %s: %v %s", name, err, out)
		}
	}
	before := w.acked.Load()
	deadline := time.Now().Add(90 * time.Second)
	for w.acked.Load() < before+20 {
		if time.Now().After(deadline) {
			t.Fatalf("no writes after the primary's node died (%d errors)", w.errs.Load())
		}
		time.Sleep(200 * time.Millisecond)
	}
	restored := time.Now()
	t.Logf("writes back through the same URL after %s; %d client errors", restored.Sub(killed).Round(100*time.Millisecond), w.errs.Load())
	if restored.Sub(killed) > 60*time.Second {
		t.Errorf("writes came back after %s, want under 60 s", restored.Sub(killed))
	}

	// Let the next minutes be measured, then read the record.
	time.Sleep(time.Until(restored.Truncate(time.Minute).Add(70 * time.Second)))
	w.Stop()
	st = haStatus(t, e, c.Project.Id)
	if l, ok := leaderOf(st); !ok || l.NodeId != ns[1].Id {
		t.Fatalf("leader after the failover: %+v", st.Members)
	}
	if len(st.Failovers) == 0 || st.Failovers[0].Kind != gen.Failover {
		t.Fatalf("failover history: %+v", st.Failovers)
	}
	a := st.Availability
	if a == nil || a.Percent == nil || a.RecentOutages == nil {
		t.Fatalf("availability: %+v", a)
	}
	first, last := killed.UTC().Truncate(time.Minute), restored.UTC().Truncate(time.Minute)
	outages := *a.RecentOutages
	if len(outages) == 0 || a.UnavailableMinutes != len(outages) {
		t.Fatalf("no unavailable minute recorded: %+v", a)
	}
	for _, m := range outages {
		if m.Minute.Before(first) || m.Minute.After(last) {
			t.Errorf("minute %s marked unavailable, outside the outage (%s to %s)", m.Minute.Format(time.TimeOnly), first.Format(time.TimeOnly), last.Format(time.TimeOnly))
		}
		if m.InternalOk == nil || *m.InternalOk || m.ExternalOk == nil || *m.ExternalOk {
			t.Errorf("minute %s: internal %v, external %v; both vantage points should have failed", m.Minute.Format(time.TimeOnly), m.InternalOk, m.ExternalOk)
		}
	}
	t.Logf("availability %s: %d of %d minutes unavailable (%.2f%%), failover recorded at %d ms",
		a.Month, a.UnavailableMinutes, a.MeasuredMinutes, *a.Percent, *st.Failovers[0].DurationMs)
}
