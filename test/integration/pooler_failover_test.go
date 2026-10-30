package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/floatip"
	"github.com/israel-duff/xenibase/internal/incidents"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/statusapi"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/test/testenv"
)

// edgeHost is one pooler-host container and its keepalived sidecar.
type edgeHost struct {
	name, container, ip, serverID string
}

// TestPoolerHostFailover is M17's done-when (V3 §2.1): two real pooler
// hosts (the pooler-host image) with keepalived on the dev Docker network,
// a shared address keepalived moves, and a fake Hetzner API for the
// floating IP. Killing the host that holds the address must bring
// connections back through it, on the other host, within 10 seconds.
func TestPoolerHostFailover(t *testing.T) {
	hostImage, kaImage := os.Getenv("XENIBASE_TEST_POOLER_HOST_IMAGE"), os.Getenv("XENIBASE_TEST_KEEPALIVED_IMAGE")
	if hostImage == "" || kaImage == "" {
		t.Skip("XENIBASE_TEST_POOLER_HOST_IMAGE and XENIBASE_TEST_KEEPALIVED_IMAGE are not set (make pooler-host-images)")
	}
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	gw := os.Getenv("XENIBASE_TEST_DOCKER_GATEWAY")
	network := os.Getenv("XENIBASE_TEST_DOCKER_NETWORK")
	if gw == "" || network == "" {
		t.Skip("XENIBASE_TEST_DOCKER_GATEWAY and XENIBASE_TEST_DOCKER_NETWORK are not set")
	}
	prefix := gw[:strings.LastIndex(gw, ".")+1]
	vip := prefix + "240"

	// The fake Hetzner API, where the arbiter (here) reaches it. The hosts
	// have no token: the arbiter moves the floating IP to keepalived's
	// MASTER.
	fake := &floatip.Fake{Token: "edge-token", IPID: "77"}
	ln, err := net.Listen("tcp", net.JoinHostPort(gw, "0"))
	if err != nil {
		t.Fatal(err)
	}
	hz := &http.Server{Handler: fake, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hz.Serve(ln) }()
	t.Cleanup(func() { _ = hz.Close() })
	hzURL := "http://" + ln.Addr().String() + "/v1"

	hosts := []*edgeHost{
		{name: "edge-a", container: "xenibase-test-edge-a", ip: prefix + "231", serverID: "1001"},
		{name: "edge-b", container: "xenibase-test-edge-b", ip: prefix + "232", serverID: "1002"},
	}
	removeEdge := func() {
		for _, h := range hosts {
			_ = exec.Command("docker", "rm", "-f", "-v", h.container+"-keepalived", h.container).Run()
		}
	}
	removeEdge()
	t.Cleanup(func() {
		if t.Failed() {
			for _, h := range hosts {
				for _, c := range []string{h.container, h.container + "-keepalived"} {
					out, _ := exec.Command("docker", "logs", "--tail", "40", c).CombinedOutput()
					t.Logf("%s:\n%s", c, out)
				}
			}
		}
		removeEdge()
		_, _ = e.DB.Exec(context.Background(), `DELETE FROM nodes WHERE role = 'pooler'`)
		_, _ = e.DB.Exec(context.Background(), `DELETE FROM pooler_generations`)
	})

	keepalivedScript, err := os.ReadFile("../../deploy/pooler-host/keepalived.sh")
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range hosts {
		var created gen.NodeCreated
		if code := e.Do("POST", "/api/v1/nodes", gen.CreateNodeRequest{Name: h.name, PrivateAddr: h.ip, Role: gen.CreateNodeRequestRolePooler}, &created); code != http.StatusCreated || created.Token == "" {
			t.Fatalf("create pooler node %s: %d", h.name, code)
		}
		edgeDir := t.TempDir()
		if err := os.Chmod(edgeDir, 0o777); err != nil {
			t.Fatal(err)
		}
		peer := hosts[1-i].ip
		run := []string{"run", "-d", "--name", h.container, "--network", network, "--ip", h.ip,
			"-v", edgeDir + ":/etc/xenibase-edge",
			"-e", "XENIBASE_AGENT_SERVER=" + e.URL, "-e", "XENIBASE_AGENT_TOKEN=" + created.Token,
			"-e", "XENIBASE_AGENT_ADVERTISE=" + h.ip + ":7070", "-e", "XENIBASE_AGENT_LOG_FORMAT=text",
			"-e", "XENIBASE_AGENT_SERVER_ID=" + h.serverID,
			"-e", "XENIBASE_EDGE_SELF_IP=" + h.ip, "-e", "XENIBASE_EDGE_PEER_IP=" + peer, "-e", "XENIBASE_EDGE_VIP=" + vip,
			"-e", "XENIBASE_EDGE_VRRP_PASSWORD=edgetest", "-e", "XENIBASE_EDGE_PRIORITY=" + strconv.Itoa(110-10*i),
			hostImage}
		if out, err := exec.Command("docker", run...).CombinedOutput(); err != nil {
			t.Fatalf("start %s: %v: %s", h.container, err, out)
		}
		ka := []string{"run", "-d", "--name", h.container + "-keepalived", "--network", "container:" + h.container,
			"--cap-add", "NET_ADMIN", "--cap-add", "NET_BROADCAST", "--cap-add", "NET_RAW",
			"-v", edgeDir + ":/etc/xenibase-edge:ro", "--entrypoint", "sh", kaImage, "-c", string(keepalivedScript)}
		if out, err := exec.Command("docker", ka...).CombinedOutput(); err != nil {
			t.Fatalf("start %s keepalived: %v: %s", h.container, err, out)
		}
	}

	q := store.New(e.DB)
	waitFor(t, 60*time.Second, "both pooler hosts to register", func() bool {
		for _, h := range hosts {
			n, err := q.GetNodeByName(ctx, h.name)
			if err != nil || n.AgentCertFp == nil {
				return false
			}
		}
		return true
	})

	// xenibase-server's side, as cmd/server wires it.
	e.Pooler.SetHostDriver(nodes.PoolerDriver{S: e.Nodes}, pooler.HostAdminConfig{
		User: "xenibase", Password: os.Getenv("XENIBASE_TEST_POOLER_ADMIN_PASSWORD"), SSLMode: "require",
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	arb := pooler.NewArbiter(e.Pooler, &floatip.Hetzner{API: hzURL, Token: fake.Token, IPID: fake.IPID}, log)
	actx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); arb.Run(actx, 3*time.Second) }()
	t.Cleanup(func() { stop(); <-done })

	creds := e.CreateProject("edge")
	if err := e.Pooler.Reload(ctx); err != nil {
		t.Fatalf("push to pooler hosts: %v", err)
	}
	u, err := url.Parse(creds.Connection.PooledUrl)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = net.JoinHostPort(vip, "6543")
	viaVIP := u.String()
	query := func() error {
		cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		defer cancel()
		c, err := pgx.Connect(cctx, viaVIP)
		if err != nil {
			return err
		}
		defer c.Close(context.Background())
		var one int
		return c.QueryRow(cctx, "SELECT 1").Scan(&one)
	}

	byServer := map[int64]*edgeHost{1001: hosts[0], 1002: hosts[1]}
	var lastErr error
	waitFor(t, 60*time.Second, "an active pooler host serving through the shared address", func() bool {
		snap := arb.Snapshot()
		ready := 0
		for _, h := range snap.Hosts {
			if h.Ready {
				ready++
			}
		}
		lastErr = query()
		return ready == 2 && byServer[fake.Assigned()] != nil && lastErr == nil
	})
	active := byServer[fake.Assigned()]
	standby := hosts[0]
	if active == hosts[0] {
		standby = hosts[1]
	}
	t.Logf("active %s, standby %s", active.name, standby.name)

	// xenibase-status, outside the dev network, probing both ports through
	// the shared address as an outside prober would (V3 §2.6).
	st := startStatusContainer(t, creds.Connection.SessionUrl, creds.Connection.PooledUrl, vip)
	st.waitEdge(t, statusapi.Operational, false)
	hb := incidents.New(e.DB, incidents.Config{URL: st.url, Secret: statusSecret}, log)
	if err := hb.Heartbeat(ctx); err != nil {
		t.Fatalf("heartbeat to the status container: %v", err)
	}

	// Kill the active host: no clean shutdown, as on a crashed machine.
	start := time.Now()
	if out, err := exec.Command("docker", "kill", active.container).CombinedOutput(); err != nil {
		t.Fatalf("kill %s: %v: %s", active.container, err, out)
	}
	for {
		lastErr = query()
		if lastErr == nil && byServer[fake.Assigned()] == standby {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("no recovery within 10s: last query error %v, floating IP on %d", lastErr, fake.Assigned())
		}
		time.Sleep(200 * time.Millisecond)
	}
	took := time.Since(start)
	t.Logf("recovered on %s in %s", standby.name, took.Round(100*time.Millisecond))

	// The arbiter saw keepalived's move and recorded it.
	n, err := q.GetNodeByName(ctx, standby.name)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "a took_ip event for the standby", func() bool {
		evs, err := q.ListPoolerEvents(ctx, 50)
		if err != nil {
			return false
		}
		for _, ev := range evs {
			if ev.Kind == "took_ip" && ev.NodeID != nil && *ev.NodeID == n.ID {
				return true
			}
		}
		return false
	})
	if snap := arb.Snapshot(); snap.HolderName != standby.name {
		t.Fatalf("arbiter sees the floating IP on %q, want %s", snap.HolderName, standby.name)
	}
	if s := st.state(t, "backups"); s != statusapi.Operational {
		t.Fatalf("backups on the status page after a heartbeat: %s", s)
	}

	// Now lose both hosts: the status page marks the edge pooler down and
	// opens an incident by itself. Bringing one back resolves it.
	st.waitEdge(t, statusapi.Operational, false)
	if out, err := exec.Command("docker", "kill", standby.container).CombinedOutput(); err != nil {
		t.Fatalf("kill %s: %v: %s", standby.container, err, out)
	}
	st.waitEdge(t, statusapi.Down, true)
	for _, c := range []string{active.container, active.container + "-keepalived"} {
		if out, err := exec.Command("docker", "restart", c).CombinedOutput(); err != nil {
			t.Fatalf("restart %s: %v: %s", c, err, out)
		}
	}
	st.waitEdge(t, statusapi.Operational, false)
	var list struct {
		Incidents []statusapi.Incident `json:"incidents"`
	}
	st.get(t, "/api/v1/incidents", &list)
	resolved := 0
	for _, in := range list.Incidents {
		if in.Auto && in.ResolvedAt != nil && slices.Contains(in.Components, "edge-pooler") {
			resolved++
		}
	}
	if resolved == 0 {
		t.Fatalf("no automatically resolved edge pooler incident: %+v", list.Incidents)
	}
}

// statusContainer is xenibase-status in Docker, on the host's network.
type statusContainer struct{ name, url string }

func startStatusContainer(t *testing.T, sessionURL, pooledURL, vip string) *statusContainer {
	t.Helper()
	image := os.Getenv("XENIBASE_TEST_STATUS_IMAGE")
	if image == "" {
		t.Skip("XENIBASE_TEST_STATUS_IMAGE is not set (make status-image)")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	through := func(raw, port string) string {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		u.Host = net.JoinHostPort(vip, port)
		return u.String()
	}
	cfg := `public_url = "http://` + addr + `"
listen = "` + addr + `"
interval = "1s"
fail_after = 3
recover_after = 2
push_secret = "` + statusSecret + `"

[[component]]
id = "edge-pooler"
name = "Edge pooler"
  [[component.probe]]
  name = "session port"
  kind = "postgres"
  dsn = "` + through(sessionURL, "5432") + `"
  timeout = "2s"
  [[component.probe]]
  name = "transaction port"
  kind = "postgres"
  dsn = "` + through(pooledURL, "6543") + `"
  timeout = "2s"

[[component]]
id = "backups"
heartbeat = true
`
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &statusContainer{name: "xenibase-test-status", url: "http://" + addr}
	_ = exec.Command("docker", "rm", "-f", "-v", c.name).Run()
	out, err := exec.Command("docker", "run", "-d", "--name", c.name, "--network", "host",
		"-e", "XENIBASE_STATUS_LOG_FORMAT=text", "-v", dir+":/etc/xenibase-status:ro", image).CombinedOutput()
	if err != nil {
		t.Fatalf("start xenibase-status: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", "--tail", "60", c.name).CombinedOutput()
			t.Logf("xenibase-status:\n%s", out)
		}
		_ = exec.Command("docker", "rm", "-f", "-v", c.name).Run()
	})
	return c
}

func (c *statusContainer) get(t *testing.T, path string, v any) bool {
	t.Helper()
	resp, err := http.Get(c.url + path)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(v) == nil
}

func (c *statusContainer) state(t *testing.T, id string) string {
	t.Helper()
	var st struct {
		Components []struct{ ID, Status string } `json:"components"`
	}
	c.get(t, "/api/v1/status", &st)
	for _, x := range st.Components {
		if x.ID == id {
			return x.Status
		}
	}
	return ""
}

// waitEdge waits for the edge pooler's state and whether an incident is
// open for it.
func (c *statusContainer) waitEdge(t *testing.T, want string, incident bool) {
	t.Helper()
	var last string
	waitFor(t, 90*time.Second, "the status page to show the edge pooler "+want, func() bool {
		var st struct {
			Components []struct{ ID, Status string } `json:"components"`
			Active     []statusapi.Incident          `json:"active_incidents"`
		}
		if !c.get(t, "/api/v1/status", &st) {
			return false
		}
		open := false
		for _, in := range st.Active {
			open = open || (in.Auto && slices.Contains(in.Components, "edge-pooler"))
		}
		for _, x := range st.Components {
			if x.ID == "edge-pooler" {
				last = x.Status
				return x.Status == want && open == incident
			}
		}
		return false
	})
	t.Logf("status page: edge pooler %s (incident open: %v)", last, incident)
}
