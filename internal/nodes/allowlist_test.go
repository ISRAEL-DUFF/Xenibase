package nodes_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/agentca"
	"github.com/israel-duff/xenibase/internal/agentsvc"
	"github.com/israel-duff/xenibase/internal/nodes"
)

// agentCall is one request a recording agent received.
type agentCall struct {
	method, path, serial string
	allowed              bool // the agent's list admitted the serial when it arrived
	status               int
}

type recordingAgent struct {
	url   string
	allow *agentsvc.ClientAllow
	mu    sync.Mutex
	calls []agentCall
}

func (r *recordingAgent) log() []agentCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentCall(nil), r.calls...)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(s int) { w.status = s; w.ResponseWriter.WriteHeader(s) }

// startRecordingAgent registers an agent with an allow list, as
// xenibase-agent runs, and records every call it gets.
func (e *env) startRecordingAgent(t *testing.T) *recordingAgent {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	st, err := agentsvc.Register(context.Background(), t.TempDir(), agentsvc.RegisterOptions{
		Server: e.reg.URL, Token: e.token, AdvertiseHost: "127.0.0.1", AdvertisePort: ln.Addr().(*net.TCPAddr).Port, Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingAgent{url: "https://" + ln.Addr().String(), allow: agentsvc.LoadClientAllow(st.Dir, st.CACert, quiet)}
	svc := agentsvc.New(agentsvc.Config{Version: "test", NodeID: st.NodeID, ClientAllow: rec.allow}, quiet)
	h := svc.Handler()
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := r.TLS.PeerCertificates[0]
		c := agentCall{method: r.Method, path: r.URL.Path, serial: agentca.Serial(peer), allowed: rec.allow.Allowed(peer)}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		c.status = sw.status
		rec.mu.Lock()
		rec.calls = append(rec.calls, c)
		rec.mu.Unlock()
	})}
	go func() { _ = srv.Serve(tls.NewListener(ln, agentsvc.TLSConfig(st.Cert, st.CA))) }()
	t.Cleanup(func() { _ = srv.Close() })
	return rec
}

// raw calls the agent's health endpoint with a client certificate of its
// own (an attacker's copy, or an unregistered one).
func (r *recordingAgent) raw(t *testing.T, ca *agentca.CA, cert tls.Certificate) int {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: ca.Pool(), ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13,
	}}}
	defer c.CloseIdleConnections()
	res, err := c.Get(r.url + agentapi.PathHealth)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode
}

func health(t *testing.T, s *nodes.Service, e *env) {
	t.Helper()
	a, err := s.ForNode(context.Background(), e.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
}

// TestAllowListPushedAndEnforced: an agent that was never given a list
// accepts any xenibase-server certificate of the CA's; the server's first
// call gives it the list of registered serials, and from then on a
// certificate whose serial is not on it is refused.
func TestAllowListPushedAndEnforced(t *testing.T) {
	e := setup(t, "")
	ag := e.startRecordingAgent(t)
	unregistered, err := e.ca.ServerClientCertFor("elsewhere", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code := ag.raw(t, e.ca, unregistered); code != http.StatusOK {
		t.Fatalf("no list yet: %d, want the CN-only acceptance of today", code)
	}
	health(t, e.svc, e)
	if have, _ := ag.allow.Enforcing(); !have {
		t.Fatal("the server's call did not give the agent a list")
	}
	if code := ag.raw(t, e.ca, unregistered); code != http.StatusForbidden {
		t.Fatalf("unregistered serial with a list: %d, want 403", code)
	}
	if code := ag.raw(t, e.ca, nodes.CurrentClientCert(e.svc)); code != http.StatusOK {
		t.Fatalf("the server's own registered certificate: %d", code)
	}
	health(t, e.svc, e)
	for _, c := range ag.log() {
		if c.serial == agentca.Serial(nodes.CurrentClientCert(e.svc).Leaf) && c.status != http.StatusOK {
			t.Fatalf("the server got %d on %s %s", c.status, c.method, c.path)
		}
	}
}

// TestRevokedServerRefused: two servers share the metadata database, and
// the agent accepts both. An admin revokes server b: a copy of b's
// certificate is refused from then on, while server b itself issues and
// registers a new one and keeps its agents. Revoking a server's serial
// from that server works the same way.
func TestRevokedServerRefused(t *testing.T) {
	e := setup(t, "")
	ag := e.startRecordingAgent(t)
	a, err := nodes.NewServiceNamed(e.db, e.ca, "server-a", quiet)
	if err != nil {
		t.Fatal(err)
	}
	b, err := nodes.NewServiceNamed(e.db, e.ca, "server-b", quiet)
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := agentca.ServerNameFromCert(nodes.CurrentClientCert(b).Leaf); !ok || name != "server-b" {
		t.Fatalf("certificate names %q", name)
	}
	health(t, a, e)
	health(t, b, e)
	stolen := nodes.CurrentClientCert(b)
	if code := ag.raw(t, e.ca, stolen); code != http.StatusOK {
		t.Fatalf("b's certificate before revocation: %d", code)
	}

	ctx := context.Background()
	if _, err := a.RevokeClientCerts(ctx, "", "", nil); !errors.Is(err, nodes.ErrInvalid) {
		t.Fatalf("revoke with nothing named: %v", err)
	}
	if _, err := a.RevokeClientCerts(ctx, "abc", "", nil); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("revoke of an unknown serial: %v", err)
	}
	res, err := a.RevokeClientCerts(ctx, "", "server-b", nil)
	if err != nil || len(res.Revoked) != 1 || res.Revoked[0].Serial != agentca.Serial(stolen.Leaf) || len(res.NotPushed) != 0 {
		t.Fatalf("revoke server-b: %+v %v", res, err)
	}
	if code := ag.raw(t, e.ca, stolen); code != http.StatusForbidden {
		t.Fatalf("revoked certificate: %d, want 403", code)
	}
	// Server a is unaffected; server b replaces its certificate on its
	// next call and is accepted again, and the copy stays refused.
	health(t, a, e)
	health(t, b, e)
	if nodes.CurrentClientCert(b).Leaf.SerialNumber.Cmp(stolen.Leaf.SerialNumber) == 0 {
		t.Fatal("server b still uses its revoked certificate")
	}
	if code := ag.raw(t, e.ca, stolen); code != http.StatusForbidden {
		t.Fatalf("revoked certificate after b renewed: %d", code)
	}

	// A server revoking its own serial reissues before pushing.
	own := nodes.CurrentClientCert(a)
	if _, err := a.RevokeClientCerts(ctx, agentca.Serial(own.Leaf), "", nil); err != nil {
		t.Fatal(err)
	}
	if nodes.CurrentClientCert(a).Leaf.SerialNumber.Cmp(own.Leaf.SerialNumber) == 0 {
		t.Fatal("server a kept its revoked certificate")
	}
	if code := ag.raw(t, e.ca, own); code != http.StatusForbidden {
		t.Fatalf("a's revoked certificate: %d", code)
	}
	health(t, a, e)
	certs, err := a.ListClientCerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current := 0
	for _, c := range certs {
		if c.Current {
			current++
		}
	}
	if current != 1 || len(certs) < 4 {
		t.Fatalf("listed %d certificates, %d current", len(certs), current)
	}
}

// TestRenewalPushedBeforeUse: a certificate past half its life is renewed
// before it expires, and every agent is given the list naming the new
// serial before any command is made with it.
func TestRenewalPushedBeforeUse(t *testing.T) {
	e := setup(t, "")
	ag := e.startRecordingAgent(t)
	health(t, e.svc, e)
	if err := nodes.SetClientCertIssuedAt(e.svc, time.Now().Add(-13*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old := nodes.CurrentClientCert(e.svc)
	// The next call finds 11 of 24 hours left: it renews, and the agent
	// must learn the new serial before the call is made with it.
	health(t, e.svc, e)
	renewed := nodes.CurrentClientCert(e.svc)
	if renewed.Leaf.SerialNumber.Cmp(old.Leaf.SerialNumber) == 0 {
		t.Fatal("not renewed with 11 of 24 hours left")
	}
	if exp := e.svc.ClientCertExpiry(); time.Until(exp) < 23*time.Hour {
		t.Fatalf("renewed certificate expires %s", exp)
	}
	health(t, e.svc, e)

	first := true
	for _, c := range ag.log() {
		if c.status != http.StatusOK {
			t.Errorf("%s %s with %s: %d", c.method, c.path, c.serial, c.status)
		}
		if c.method != http.MethodPut && !c.allowed {
			t.Errorf("%s %s was made with %s before the agent's list named it", c.method, c.path, c.serial)
		}
		if c.serial == agentca.Serial(renewed.Leaf) && first {
			first = false
			if c.method != http.MethodPut || c.path != agentapi.PathClientSerials {
				t.Errorf("the new certificate was first used for %s %s, not to push the list", c.method, c.path)
			}
		}
	}
	if first {
		t.Fatal("the renewed certificate was never used")
	}
	// The old certificate stays on the list until it expires: calls in
	// flight with it are not cut off.
	if code := ag.raw(t, e.ca, old); code != http.StatusOK {
		t.Fatalf("the replaced, unexpired certificate: %d", code)
	}
}
