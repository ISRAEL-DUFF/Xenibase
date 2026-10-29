package integration

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/test/testenv"
)

// newerMinorImage points the Postgres 17 image tag at a copy that reports
// release 17.99, as pulling a new minor release would, and puts the tag
// back when the test ends.
func newerMinorImage(t *testing.T) {
	t.Helper()
	tmpl := os.Getenv("XENIBASE_TEST_PG_IMAGE")
	if !strings.Contains(tmpl, "{major}") {
		t.Skip("XENIBASE_TEST_PG_IMAGE is not a {major} template")
	}
	tag := strings.ReplaceAll(tmpl, "{major}", "17")
	orig, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}} {{range .Config.Env}}{{.}} {{end}}", tag).Output()
	if err != nil {
		t.Fatalf("inspect %s: %v", tag, err)
	}
	if strings.Contains(string(orig), "PG_VERSION=17.99-test") {
		t.Fatalf("%s is the test image left by an interrupted run: rebuild it (make pg-image) or tag the original back", tag)
	}
	orig = []byte(strings.Fields(string(orig))[0])
	// FROM takes a name, not an image ID.
	const base = "xenibase-minor-test-base:latest"
	if out, err := exec.Command("docker", "tag", tag, base).CombinedOutput(); err != nil {
		t.Fatalf("tag %s: %v\n%s", base, err, out)
	}
	defer func() { _ = exec.Command("docker", "rmi", base).Run() }()
	build := exec.Command("docker", "build", "-q", "-t", tag, "-")
	build.Stdin = strings.NewReader("FROM " + base + "\nENV PG_VERSION=17.99-test\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the newer image: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "tag", strings.TrimSpace(string(orig)), tag).CombinedOutput(); err != nil {
			t.Errorf("restore %s: %v\n%s", tag, err, out)
		}
	})
}

// TestMinorUpgradeInWindow: in the maintenance window, the sweep restarts
// a dedicated instance and then a shared cluster onto a newer minor
// release, one per sweep, while clients keep writing through the poolers.
func TestMinorUpgradeInWindow(t *testing.T) {
	needDedicated(t)
	e := testenv.Start(t, testenv.Options{})
	e.StartAgent()
	e.ConfigureBackups()
	e.SetNodeRole("test", "both")
	ctx := context.Background()
	v17 := 17

	var created gen.NodeCreated
	e.Do("POST", "/api/v1/nodes", gen.CreateNodeRequest{Name: "node-17", PrivateAddr: "agent-test-2", Role: gen.CreateNodeRequestRoleShared}, &created)
	e.StartSecondAgent("node-17", created.Token)
	var op gen.Operation
	if code := e.Do("POST", "/api/v1/nodes/"+created.Node.Id.String()+"/shared-cluster", gen.SharedClusterRequest{MemoryMb: 512, PgVersion: &v17}, &op); code != http.StatusAccepted {
		t.Fatalf("shared cluster: %d", code)
	}
	if op = e.WaitOperation(op.Id); op.Status != gen.OperationStatusSucceeded {
		t.Fatalf("Postgres 17 shared cluster: %s\n%s", op.Status, testenv.FormatLog(op))
	}
	create := func(req gen.CreateProjectRequest) gen.ProjectCredentials {
		var c gen.ProjectCredentials
		if code := e.Do("POST", "/api/v1/projects", req, &c); code != http.StatusAccepted {
			t.Fatalf("create %s: %d", req.Name, code)
		}
		if op := e.WaitOperation(c.Operation.Id); op.Status != gen.OperationStatusSucceeded {
			t.Fatalf("create %s: %s\n%s", req.Name, op.Status, testenv.FormatLog(op))
		}
		return c
	}
	tier, profile, vol := gen.ProjectTierDedicated, "small", 5
	ded := create(gen.CreateProjectRequest{Name: "Dedicated minor", Tier: &tier, Profile: &profile, VolumeGb: &vol, PgVersion: &v17})
	shared := create(gen.CreateProjectRequest{Name: "Shared minor", PgVersion: &v17})

	maintenance := func() gen.MaintenanceStatus {
		var st gen.MaintenanceStatus
		if code := e.Do("GET", "/api/v1/admin/maintenance", nil, &st); code != http.StatusOK {
			t.Fatalf("maintenance: %d", code)
		}
		return st
	}
	if err := e.Dedicated.CheckReleases(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st := maintenance(); len(st.Behind) != 0 {
		t.Fatalf("behind before a new image: %+v", st.Behind)
	}

	newerMinorImage(t)
	if err := e.Dedicated.CheckReleases(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	st := maintenance()
	if len(st.Behind) != 2 {
		t.Fatalf("behind = %+v, want the dedicated instance and the shared cluster", st.Behind)
	}
	for _, b := range st.Behind {
		if b.PgReleaseAvailable == nil || *b.PgReleaseAvailable != "17.99" || b.PgRelease == nil || !strings.HasPrefix(*b.PgRelease, "17.") {
			t.Fatalf("releases: %+v", b)
		}
	}

	// Outside the window nothing restarts.
	now := time.Now().UTC()
	away := gen.MaintenanceWindow{Enabled: true, Weekday: (int(now.Weekday()) + 3) % 7, StartHour: 2, Hours: 4}
	if code := e.Do("PUT", "/api/v1/admin/maintenance/window", away, nil); code != http.StatusOK {
		t.Fatalf("window: %d", code)
	}
	if inst, err := e.Dedicated.MaintenanceSweep(ctx, now); err != nil || inst != nil {
		t.Fatalf("sweep outside the window: upgraded %v: %v", inst != nil, err)
	}
	if code := e.Do("PUT", "/api/v1/admin/maintenance/window", gen.MaintenanceWindow{Hours: 30}, nil); code != http.StatusBadRequest {
		t.Fatalf("a 30-hour window: %d", code)
	}

	here := gen.MaintenanceWindow{Enabled: true, Weekday: int(now.Weekday()), StartHour: now.Hour(), Hours: 2}
	if code := e.Do("PUT", "/api/v1/admin/maintenance/window", here, nil); code != http.StatusOK {
		t.Fatalf("window: %d", code)
	}
	if st := maintenance(); !st.InWindow {
		t.Fatalf("not in the window: %+v", st.Window)
	}

	wd := startWriter(t, e, ded.Connection.PooledUrl)
	ws := startWriter(t, e, shared.Connection.PooledUrl)
	time.Sleep(time.Second)
	var kinds []string
	for range 2 {
		up, err := e.Dedicated.MaintenanceSweep(ctx, time.Now())
		if err != nil || up == nil {
			t.Fatalf("sweep %d: upgraded %v: %v", len(kinds)+1, up != nil, err)
		}
		// One at a time: nothing more is queued while it runs.
		if more, err := e.Dedicated.MaintenanceSweep(ctx, time.Now()); err != nil || more != nil {
			t.Fatalf("a sweep during an upgrade queued %v: %v", more != nil, err)
		}
		if op := e.WaitOperation(up.Operation.ID); op.Status != gen.OperationStatusSucceeded {
			t.Fatalf("%s minor upgrade: %s\n%s", up.Instance.Kind, op.Status, testenv.FormatLog(op))
		}
		kinds = append(kinds, up.Instance.Kind)
		time.Sleep(500 * time.Millisecond)
	}
	if inst, err := e.Dedicated.MaintenanceSweep(ctx, time.Now()); err != nil || inst != nil {
		t.Fatalf("a third sweep: upgraded %v: %v", inst != nil, err)
	}
	wd.Stop()
	ws.Stop()
	if strings.Join(kinds, ",") != "dedicated,shared" {
		t.Fatalf("upgrade order = %v, want the dedicated instance first", kinds)
	}

	st = maintenance()
	if len(st.Behind) != 0 || len(st.History) != 2 {
		t.Fatalf("after the sweeps: behind %+v, history %+v", st.Behind, st.History)
	}
	for _, h := range st.History {
		if h.Error != nil || h.ToRelease != "17.99" || h.PauseMs == nil || h.FinishedAt == nil {
			t.Fatalf("history: %+v", h)
		}
		t.Logf("%s on %s: %s → %s, clients held %d ms", h.Kind, h.NodeName, h.FromRelease, h.ToRelease, *h.PauseMs)
	}
	for name, c := range map[string]struct {
		url string
		w   *liveWriter
	}{"dedicated": {ded.Connection.PooledUrl, wd}, "shared": {shared.Connection.PooledUrl, ws}} {
		conn := e.MustConnect(c.url)
		var count, maxN int64
		err := conn.QueryRow(ctx, `SELECT count(*), coalesce(max(n), 0) FROM ledger`).Scan(&count, &maxN)
		_ = conn.Close(ctx)
		if err != nil || maxN < c.w.acked.Load() || count != maxN {
			t.Fatalf("%s ledger: %d rows max %d (acked %d) %v", name, count, maxN, c.w.acked.Load(), err)
		}
		if v := serverMajor(t, e, c.url); v != 17 {
			t.Fatalf("%s runs Postgres %d after a minor upgrade", name, v)
		}
	}

	// Run now: refused once the instance is up to date.
	var p gen.Project
	e.Do("GET", "/api/v1/projects/"+ded.Project.Id.String(), nil, &p)
	if code := e.Do("POST", "/api/v1/admin/instances/"+p.Instance.Id.String()+"/minor-upgrade", nil, nil); code != http.StatusConflict {
		t.Fatalf("run now on an up-to-date instance: %d", code)
	}
}
