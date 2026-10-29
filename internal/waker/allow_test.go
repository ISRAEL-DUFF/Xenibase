package waker_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"

	"github.com/israel-duff/xenibase/internal/waker"
)

// TestRefusesOutsideThePoolers: the waker doesn't authenticate, so a
// connection from an address Allow doesn't admit is refused before its
// startup packet is read, and wakes nothing.
func TestRefusesOutsideThePoolers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw := &fakeWaker{state: map[string]waker.State{"p_aaaaaaaaaa": waker.Resuming}}
	s := waker.New(fw, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var asked []netip.Addr
	s.Allow = func(a netip.Addr) bool { asked = append(asked, a); return false }
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	pe := connect(t, ln.Addr().String(), "p_aaaaaaaaaa", "disable")
	if pe.Code != "28000" {
		t.Fatalf("refused with %s %s, want 28000", pe.Code, pe.Message)
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if len(fw.woken) != 0 {
		t.Fatalf("woke %v from a refused address", fw.woken)
	}
	if len(asked) != 1 || asked[0] != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("Allow was asked about %v", asked)
	}
}

func TestAllowList(t *testing.T) {
	calls := 0
	allow := waker.AllowList(func() []netip.Addr {
		calls++
		return []netip.Addr{netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("2001:db8::7")}
	}, []netip.Prefix{netip.MustParsePrefix("172.31.250.0/24")})
	for _, c := range []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1", true},       // the server's own poolers next to it
		{"::1", true},             // loopback
		{"10.0.0.5", true},        // a pooler host
		{"::ffff:10.0.0.5", true}, // the same, IPv4-mapped
		{"2001:db8::7", true},     // a pooler host over IPv6
		{"172.31.250.20", true},   // XENIBASE_WAKER_ALLOW
		{"10.0.0.6", false},       // another machine on the private network
		{"203.0.113.9", false},    // the internet
	} {
		if got := allow(netip.MustParseAddr(c.addr)); got != c.ok {
			t.Errorf("%s: %v, want %v", c.addr, got, c.ok)
		}
	}
	if calls > 2 {
		t.Errorf("the pooler addresses were listed %d times for one burst", calls)
	}
}
