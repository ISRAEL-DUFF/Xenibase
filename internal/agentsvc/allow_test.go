package agentsvc

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/agentapi"
	"github.com/israel-duff/xenibase/internal/agentca"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// allowAgent is a real agent API behind the agent's TLS configuration.
type allowAgent struct {
	ca    *agentca.CA
	allow *ClientAllow
	dir   string
	url   string
	stop  func()
}

func startAllowAgent(t *testing.T, ca *agentca.CA, dir string) *allowAgent {
	t.Helper()
	csr, keyPEM, err := agentca.NewCSR()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := ca.SignAgent(csr, "node-1", []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	allow := LoadClientAllow(dir, ca.Cert(), quietLog)
	svc := New(Config{Version: "test", NodeID: "node-1", ClientAllow: allow}, quietLog)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: svc.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(tls.NewListener(ln, TLSConfig(cert, ca.Pool()))) }()
	a := &allowAgent{ca: ca, allow: allow, dir: dir, url: "https://" + ln.Addr().String(), stop: func() { _ = srv.Close() }}
	t.Cleanup(a.stop)
	return a
}

// client presents cert; it keeps its connections alive.
func (a *allowAgent) client(cert tls.Certificate) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: a.ca.Pool(), MinVersion: tls.VersionTLS13, ServerName: "127.0.0.1",
	}}}
}

func health(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	res, err := c.Get(url + agentapi.PathHealth)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode
}

func push(t *testing.T, c *http.Client, url string, cs agentapi.ClientSerials) (int, agentapi.ClientSerialsAck) {
	t.Helper()
	b, _ := json.Marshal(cs)
	req, _ := http.NewRequest(http.MethodPut, url+agentapi.PathClientSerials, bytes.NewReader(b))
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var ack agentapi.ClientSerialsAck
	_ = json.NewDecoder(res.Body).Decode(&ack)
	return res.StatusCode, ack
}

func signed(t *testing.T, ca *agentca.CA, gen int64, certs ...tls.Certificate) agentapi.ClientSerials {
	t.Helper()
	l := agentapi.ClientSerialList{Generation: gen, SignedAt: time.Now()}
	for _, c := range certs {
		name, _ := agentca.ServerNameFromCert(c.Leaf)
		l.Serials = append(l.Serials, agentapi.ClientSerial{Serial: agentca.Serial(c.Leaf), Server: name, ExpiresAt: c.Leaf.NotAfter})
	}
	cs, err := ca.SignList(l)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func issue(t *testing.T, ca *agentca.CA, name string) tls.Certificate {
	t.Helper()
	c, err := ca.ServerClientCertFor(name, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestAllowListNoListAcceptsByCN: an agent that was never given a list
// accepts any client certificate the CA signed for xenibase-server, as
// agents always did (an older server, or the first call of a new one).
func TestAllowListNoListAcceptsByCN(t *testing.T) {
	ca, _ := agentca.New()
	a := startAllowAgent(t, ca, t.TempDir())
	if have, _ := a.allow.Enforcing(); have {
		t.Fatal("a fresh agent has a list")
	}
	for _, name := range []string{"a", "b", ""} {
		if code := health(t, a.client(issue(t, ca, name)), a.url); code != http.StatusOK {
			t.Fatalf("server %q: %d without a list", name, code)
		}
	}
	// The CA check still applies with no list.
	rogue, _ := agentca.New()
	if resp, err := a.client(issue(t, rogue, "a")).Get(a.url + agentapi.PathHealth); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a certificate from another CA was accepted")
	}
}

// TestAllowListRefusesUnlisted: once an agent has a list, a valid
// xenibase-server certificate whose serial is not on it is refused, on
// every path, and that survives a restart of the agent.
func TestAllowListRefusesUnlisted(t *testing.T) {
	ca, _ := agentca.New()
	dir := t.TempDir()
	a := startAllowAgent(t, ca, dir)
	listed, unlisted := issue(t, ca, "a"), issue(t, ca, "b")

	// The first push comes from a server whose certificate the list names.
	if code, ack := push(t, a.client(listed), a.url, signed(t, ca, 1, listed)); code != http.StatusOK || ack.Generation != 1 {
		t.Fatalf("push: %d %+v", code, ack)
	}
	if code := health(t, a.client(listed), a.url); code != http.StatusOK {
		t.Fatalf("listed: %d", code)
	}
	if code := health(t, a.client(unlisted), a.url); code != http.StatusForbidden {
		t.Fatalf("unlisted serial: %d, want 403", code)
	}
	// An unlisted certificate cannot push a list that leaves it out, nor
	// one the CA did not sign; an older list is refused.
	if code, _ := push(t, a.client(unlisted), a.url, signed(t, ca, 2, listed)); code != http.StatusForbidden {
		t.Fatalf("unlisted pushed a list without itself: %d", code)
	}
	rogue, _ := agentca.New()
	if code, _ := push(t, a.client(listed), a.url, signed(t, rogue, 9, listed, unlisted)); code != http.StatusBadRequest {
		t.Fatalf("a list from another CA: %d", code)
	}
	if code, ack := push(t, a.client(listed), a.url, signed(t, ca, 0, listed, unlisted)); code != http.StatusConflict || ack.Generation != 1 {
		t.Fatalf("an older list: %d %+v", code, ack)
	}
	if code := health(t, a.client(unlisted), a.url); code != http.StatusForbidden {
		t.Fatalf("unlisted after refused pushes: %d", code)
	}

	// A restarted agent enforces the list it saved.
	if fi, err := os.Stat(filepath.Join(dir, allowFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved list: %v %v", fi, err)
	}
	a.stop()
	b := startAllowAgent(t, ca, dir)
	if have, gen := b.allow.Enforcing(); !have || gen != 1 {
		t.Fatalf("after restart: list %v, generation %d", have, gen)
	}
	if code := health(t, b.client(unlisted), b.url); code != http.StatusForbidden {
		t.Fatalf("unlisted after restart: %d", code)
	}
	if code := health(t, b.client(listed), b.url); code != http.StatusOK {
		t.Fatalf("listed after restart: %d", code)
	}

	// A damaged saved list enforces an empty one: only a push gets in.
	if err := os.WriteFile(filepath.Join(dir, allowFile), []byte(`{"list":"e30=","signature":"AAAA"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b.stop()
	c := startAllowAgent(t, ca, dir)
	if code := health(t, c.client(listed), c.url); code != http.StatusForbidden {
		t.Fatalf("with a damaged list: %d", code)
	}
	if code, _ := push(t, c.client(listed), c.url, signed(t, ca, 2, listed)); code != http.StatusOK {
		t.Fatalf("repairing push: %d", code)
	}
}

// TestAllowListRevocation: a serial dropped from the list (revoked) is
// refused at once, also on a connection it opened earlier, and a server
// with a new certificate gets it accepted by pushing a newer list naming
// it.
func TestAllowListRevocation(t *testing.T) {
	ca, _ := agentca.New()
	a := startAllowAgent(t, ca, t.TempDir())
	good, stolen := issue(t, ca, "a"), issue(t, ca, "b")
	if code, _ := push(t, a.client(good), a.url, signed(t, ca, 1, good, stolen)); code != http.StatusOK {
		t.Fatalf("push: %d", code)
	}
	thief := a.client(stolen)
	if code := health(t, thief, a.url); code != http.StatusOK {
		t.Fatalf("before revocation: %d", code)
	}
	// Server b's certificate is revoked: the next list leaves it out.
	if code, _ := push(t, a.client(good), a.url, signed(t, ca, 2, good)); code != http.StatusOK {
		t.Fatalf("push: %d", code)
	}
	if code := health(t, thief, a.url); code != http.StatusForbidden {
		t.Fatalf("revoked serial on its open connection: %d", code)
	}
	if code := health(t, a.client(stolen), a.url); code != http.StatusForbidden {
		t.Fatalf("revoked serial on a new connection: %d", code)
	}
	// The thief cannot put itself back by replaying the list that named it.
	if code, _ := push(t, thief, a.url, signed(t, ca, 1, good, stolen)); code != http.StatusConflict {
		t.Fatalf("replayed older list: %d", code)
	}
	// Server b, restarted with a new key, pushes the next list naming its
	// new serial, then calls with it.
	renewed := issue(t, ca, "b")
	if code, _ := push(t, a.client(renewed), a.url, signed(t, ca, 3, good, renewed)); code != http.StatusOK {
		t.Fatalf("new certificate's push: %d", code)
	}
	if code := health(t, a.client(renewed), a.url); code != http.StatusOK {
		t.Fatalf("new certificate: %d", code)
	}
	if code := health(t, thief, a.url); code != http.StatusForbidden {
		t.Fatalf("revoked serial after the new push: %d", code)
	}
}

// An expired entry on the list no longer admits its certificate, even on
// a connection opened while it was valid.
func TestAllowListExpiry(t *testing.T) {
	ca, _ := agentca.New()
	a := startAllowAgent(t, ca, t.TempDir())
	c := issue(t, ca, "a")
	if code, _ := push(t, a.client(c), a.url, signed(t, ca, 1, c)); code != http.StatusOK {
		t.Fatalf("push: %d", code)
	}
	cl := a.client(c)
	if code := health(t, cl, a.url); code != http.StatusOK {
		t.Fatalf("valid: %d", code)
	}
	a.allow.mu.Lock()
	a.allow.now = func() time.Time { return c.Leaf.NotAfter.Add(time.Minute) }
	a.allow.mu.Unlock()
	if code := health(t, cl, a.url); code != http.StatusForbidden {
		t.Fatalf("after its expiry: %d", code)
	}
}

// TestAllowListLegacyWindow: a certificate that names no server comes from
// a xenibase-server from before allow lists, which registers nothing. With
// a list, the agent still accepts one for LegacyClientCertWindow after
// its first list (servers are upgraded one at a time), and not after.
// Named certificates are held to the list from the start.
func TestAllowListLegacyWindow(t *testing.T) {
	ca, _ := agentca.New()
	dir := t.TempDir()
	a := startAllowAgent(t, ca, dir)
	listed, legacy, named := issue(t, ca, "a"), issue(t, ca, ""), issue(t, ca, "b")
	if code, _ := push(t, a.client(listed), a.url, signed(t, ca, 1, listed)); code != http.StatusOK {
		t.Fatalf("push: %d", code)
	}
	if code := health(t, a.client(legacy), a.url); code != http.StatusOK {
		t.Fatalf("an older server's certificate in the window: %d", code)
	}
	if code := health(t, a.client(named), a.url); code != http.StatusForbidden {
		t.Fatalf("a named, unlisted certificate in the window: %d", code)
	}
	// The window runs from the first list, across restarts and pushes.
	a.allow.mu.RLock()
	since := a.allow.since
	a.allow.mu.RUnlock()
	a.stop()
	b := startAllowAgent(t, ca, dir)
	if code, _ := push(t, b.client(listed), b.url, signed(t, ca, 2, listed)); code != http.StatusOK {
		t.Fatalf("push: %d", code)
	}
	b.allow.mu.Lock()
	if !b.allow.since.Equal(since) {
		t.Errorf("window restarted: %s, first list %s", b.allow.since, since)
	}
	b.allow.now = func() time.Time { return since.Add(LegacyClientCertWindow + time.Minute) }
	b.allow.mu.Unlock()
	if code := health(t, b.client(legacy), b.url); code != http.StatusForbidden {
		t.Fatalf("an older server's certificate after the window: %d", code)
	}
}
