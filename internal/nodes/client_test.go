package nodes_test

import (
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/israel-duff/xenibase/internal/agentsvc"
)

// countingListener counts the connections an agent accepts.
type countingListener struct {
	net.Listener
	n atomic.Int32
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

// TestAgentConnectionsAreReused: calls to one node's agent share a
// connection instead of a TLS handshake each, and a changed pin takes
// effect at once.
func TestAgentConnectionsAreReused(t *testing.T) {
	e := setup(t, "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingListener{Listener: ln}
	st, err := agentsvc.Register(context.Background(), t.TempDir(), agentsvc.RegisterOptions{
		Server: e.reg.URL, Token: e.token, AdvertiseHost: "127.0.0.1", AdvertisePort: ln.Addr().(*net.TCPAddr).Port, Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := agentsvc.New(agentsvc.Config{Version: "test", NodeID: st.NodeID, PGBinDir: os.Getenv("XENIBASE_TEST_PG_BIN"), ClientAllow: agentsvc.LoadClientAllow(st.Dir, st.CACert, quiet)}, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = agent.Serve(ctx, counted, agentsvc.TLSConfig(st.Cert, st.CA)) }()

	for i := 0; i < 5; i++ {
		a, err := e.svc.ForNode(ctx, e.node.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Health(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := counted.n.Load(); n != 1 {
		t.Fatalf("five calls opened %d connections, want 1", n)
	}

	// Another pin: the cached client is replaced, and the agent refused.
	var pin string
	if err := e.db.QueryRow(ctx, `SELECT agent_cert_fp FROM nodes WHERE id = $1`, e.node.ID).Scan(&pin); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(ctx, `UPDATE nodes SET agent_cert_fp = 'sha256:00' WHERE id = $1`, e.node.ID); err != nil {
		t.Fatal(err)
	}
	a, _ := e.svc.ForNode(ctx, e.node.ID)
	if _, err := a.Health(ctx); err == nil || !strings.Contains(err.Error(), "pinned fingerprint") {
		t.Fatalf("after the pin changed: %v", err)
	}
	if _, err := e.db.Exec(ctx, `UPDATE nodes SET agent_cert_fp = $2 WHERE id = $1`, e.node.ID, pin); err != nil {
		t.Fatal(err)
	}
	a, _ = e.svc.ForNode(ctx, e.node.ID)
	if _, err := a.Health(ctx); err != nil {
		t.Fatalf("with the pin back: %v", err)
	}
}
