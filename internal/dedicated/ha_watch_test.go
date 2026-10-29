package dedicated_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/crypto"
	"github.com/israel-duff/xenibase/internal/dedicated"
	"github.com/israel-duff/xenibase/internal/ha"
	"github.com/israel-duff/xenibase/internal/nodes"
	"github.com/israel-duff/xenibase/internal/pooler"
	"github.com/israel-duff/xenibase/internal/provision"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

func member(name, role, state string) ha.ClusterMember {
	return ha.ClusterMember{Name: name, Role: role, State: state}
}

func TestAgreedLeader(t *testing.T) {
	a, b, c := "a", "b", "c"
	view := func(ms ...ha.ClusterMember) ha.Cluster { return ha.Cluster{Members: ms} }
	for _, tc := range []struct {
		name    string
		views   []ha.Cluster
		current string
		want    string // "" no leader
		known   bool
	}{
		{"no member answers", nil, a, "", false},
		// The control plane can't reach the leader, but the others still
		// list it: Patroni hasn't dropped it, so it is still the leader.
		{"leader unreachable, others list it", []ha.Cluster{
			view(member(a, "leader", "running"), member(b, "replica", "streaming"), member(c, "replica", "streaming")),
			view(member(a, "leader", "running"), member(b, "replica", "streaming"), member(c, "replica", "streaming")),
		}, a, a, true},
		{"the others have dropped it", []ha.Cluster{
			view(member(b, "replica", "running"), member(c, "replica", "running")),
			view(member(b, "replica", "running"), member(c, "replica", "running")),
		}, a, "", true},
		{"a leader that isn't running", []ha.Cluster{view(member(a, "leader", "stopped"), member(b, "replica", "running"))}, a, "", true},
		{"a new leader", []ha.Cluster{
			view(member(b, "leader", "running"), member(c, "replica", "streaming")),
			view(member(b, "leader", "running"), member(c, "replica", "streaming")),
		}, a, b, true},
		// The old leader still says it leads; the two others agree on b.
		{"most views win", []ha.Cluster{
			view(member(a, "leader", "running")),
			view(member(b, "leader", "running"), member(c, "replica", "streaming")),
			view(member(b, "leader", "running"), member(c, "replica", "streaming")),
		}, a, b, true},
		{"a tie keeps the current leader", []ha.Cluster{
			view(member(b, "leader", "running")),
			view(member(a, "leader", "running")),
		}, a, a, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := dedicated.AgreedLeader(tc.views, tc.current)
			name := ""
			if got != nil {
				name = got.Name
			}
			if name != tc.want || known != tc.known {
				t.Fatalf("leader %q known %v, want %q %v", name, known, tc.want, tc.known)
			}
		})
	}
}

// patroniStub serves /cluster for one member; down makes it refuse.
type patroniStub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	view ha.Cluster
	down bool
}

func newPatroniStub(t *testing.T) *patroniStub {
	p := &patroniStub{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.down {
			// Hijack and drop: as a member that no longer answers.
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
					return
				}
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(p.view)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *patroniStub) set(view ha.Cluster, down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.view, p.down = view, down
}

func (p *patroniStub) addr(t *testing.T) (string, int32) {
	host, port, err := net.SplitHostPort(p.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	return host, int32(n)
}

type haEnv struct {
	*env
	inst    store.Instance
	members [3]uuid.UUID
	stubs   [3]*patroniStub
	kr      *crypto.Keyring
	ns      *nodes.Service
}

// server is another xenibase-server's dedicated service on the database.
func (e *haEnv) server(t *testing.T) *dedicated.Service {
	t.Helper()
	pm, err := pooler.NewManager(t.TempDir(), 0o644, e.db, nil, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ps := provision.NewService(e.db, e.kr, pm, provision.Config{AdminSSLMode: "disable"}, quiet)
	return dedicated.New(e.db, e.kr, e.ns, ps, secrets{}, dedicated.Config{}, quiet)
}

// setupHA is an HA instance of three members, each with a stub Patroni,
// and a project on it; members[0] leads.
func setupHA(t *testing.T) *haEnv {
	t.Helper()
	ctx := context.Background()
	db := storetest.New(t)
	k, _ := crypto.GenerateKey()
	kr, _ := crypto.NewKeyring(k)
	ca, err := agentca.LoadOrCreate(ctx, db, kr)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := nodes.NewService(db, ca, "", quiet)
	if err != nil {
		t.Fatal(err)
	}
	e := &haEnv{kr: kr, ns: ns}
	e.env = &env{db: db}
	e.svc = e.server(t)
	node := e.node(t, "node1", "healthy")
	e.inst = e.instance(t, node, "dedicated", "1 hour")
	q := store.New(db)
	for i := range e.members {
		e.members[i] = uuid.New()
		e.stubs[i] = newPatroniStub(t)
		mnode := node
		if i > 0 {
			mnode = e.node(t, "node"+strconv.Itoa(i+1), "healthy")
		}
		if _, err := q.InsertInstanceMember(ctx, store.InsertInstanceMemberParams{ID: e.members[i], InstanceID: e.inst.ID, NodeID: mnode.ID, Role: "replica"}); err != nil {
			t.Fatal(err)
		}
		host, port := e.stubs[i].addr(t)
		pgPort := int32(5432)
		if err := q.SetMemberAddress(ctx, store.SetMemberAddressParams{ID: e.members[i], Host: &host, Port: &pgPort, RestHost: &host, RestPort: &port}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `UPDATE instances SET status = 'running', patroni = true, ha_enabled = true, leader_member = $2 WHERE id = $1`,
		e.inst.ID, e.members[0]); err != nil {
		t.Fatal(err)
	}
	e.project(t, e.inst, "haproj", false)
	return e
}

func (e *haEnv) cluster(leader int, standbys ...int) ha.Cluster {
	var c ha.Cluster
	if leader >= 0 {
		c.Members = append(c.Members, member(e.members[leader].String(), "leader", "running"))
	}
	for _, i := range standbys {
		c.Members = append(c.Members, member(e.members[i].String(), "replica", "streaming"))
	}
	return c
}

// TestWatcherWaitsForPatroniToDropTheLeader: a leader whose REST API
// xenibase-server can't reach, while the other members still list it,
// leaves the poolers alone; they pause only once the members agree there
// is no leader.
func TestWatcherWaitsForPatroniToDropTheLeader(t *testing.T) {
	e := setupHA(t)
	ctx := context.Background()
	e.svc.LeadHA()
	poll := func(n int) {
		t.Helper()
		for range n {
			if err := e.svc.WatchOne(ctx, e.inst.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The leader no longer answers the control plane; the standbys still
	// see it lead.
	e.stubs[0].set(ha.Cluster{}, true)
	e.stubs[1].set(e.cluster(0, 1, 2), false)
	e.stubs[2].set(e.cluster(0, 1, 2), false)
	poll(4)
	if e.svc.WatchPaused(e.inst.ID) {
		t.Fatal("paused the poolers while Patroni still lists the leader")
	}
	// No member answers at all: nothing to go on.
	e.stubs[1].set(ha.Cluster{}, true)
	e.stubs[2].set(ha.Cluster{}, true)
	poll(4)
	if e.svc.WatchPaused(e.inst.ID) {
		t.Fatal("paused the poolers when no member answers")
	}
	// Patroni drops the leader: the standbys list none.
	e.stubs[1].set(e.cluster(-1, 1, 2), false)
	e.stubs[2].set(e.cluster(-1, 1, 2), false)
	poll(2)
	if !e.svc.WatchPaused(e.inst.ID) {
		t.Fatal("poolers not paused once the members agree there is no leader")
	}
	// It comes back.
	e.stubs[0].set(e.cluster(0, 1, 2), false)
	e.stubs[1].set(e.cluster(0, 1, 2), false)
	poll(1)
	if e.svc.WatchPaused(e.inst.ID) {
		t.Fatal("poolers still paused with the leader back")
	}
}

// TestWatcherSkipsAMarkedSwitchover: a switchover running on another
// server keeps the watcher off the instance.
func TestWatcherSkipsAMarkedSwitchover(t *testing.T) {
	e := setupHA(t)
	ctx := context.Background()
	e.svc.LeadHA()
	now := time.Now()
	if err := store.New(e.db).SetInstanceSwitchover(ctx, store.SetInstanceSwitchoverParams{ID: e.inst.ID, StartedAt: &now}); err != nil {
		t.Fatal(err)
	}
	for i := range e.stubs {
		e.stubs[i].set(e.cluster(-1, 0, 1, 2), false)
	}
	for range 3 {
		if err := e.svc.WatchOne(ctx, e.inst.ID); err != nil {
			t.Fatal(err)
		}
	}
	if e.svc.WatchPaused(e.inst.ID) {
		t.Fatal("paused the poolers during another server's switchover")
	}
}

// TestHAWatcherRunsOnOneServer: two servers on one metadata database; one
// watches, and when its lock's session ends it lets go of the poolers it
// paused and a server takes over.
func TestHAWatcherRunsOnOneServer(t *testing.T) {
	e := setupHA(t)
	defer dedicated.SetHALockTiming(50*time.Millisecond, time.Second)()
	other := e.server(t)
	var resumed atomic.Int32
	for _, s := range []*dedicated.Service{e.svc, other} {
		s.SetWatchResume(func(context.Context, ...string) error { resumed.Add(1); return nil })
	}
	// The members agree there is no leader: whoever watches pauses.
	for i := range e.stubs {
		e.stubs[i].set(e.cluster(-1, 0, 1, 2), false)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	run := func(s *dedicated.Service) {
		wg.Add(1)
		go func() { defer wg.Done(); s.RunHAWatcher(ctx, 20*time.Millisecond) }()
	}
	run(e.svc)
	waitFor(t, "the first server to lead", func() bool { return e.svc.LeadingHAWatcher() })
	run(other)
	waitFor(t, "the poolers to pause", func() bool { return e.svc.WatchPaused(e.inst.ID) })
	time.Sleep(300 * time.Millisecond)
	if other.LeadingHAWatcher() || other.WatchPaused(e.inst.ID) {
		t.Fatal("the second server watches too")
	}

	// End the first server's lock session, as a lost connection would.
	var n int
	if err := e.db.QueryRow(context.Background(), `SELECT count(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database()) AND ((classid::bigint << 32) | objid::bigint) = $1`, int64(0x7067646f636b20)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("terminate the lock holder: %d %v", n, err)
	}
	waitFor(t, "the first server to let the poolers go", func() bool { return resumed.Load() >= 1 })
	waitFor(t, "a server to take the lock", func() bool { return e.svc.LeadingHAWatcher() || other.LeadingHAWatcher() })
	if e.svc.LeadingHAWatcher() && other.LeadingHAWatcher() {
		t.Fatal("both servers lead")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
