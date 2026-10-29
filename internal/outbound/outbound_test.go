package outbound

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testService(addrs map[string][]string, allow ...string) *Service {
	return New(nil, Config{
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, a := range addrs[host] {
				out = append(out, netip.MustParseAddr(a))
			}
			if len(out) == 0 {
				return nil, errors.New("no such host")
			}
			return out, nil
		},
		Allowed: func(_ uuid.UUID, host string) bool {
			for _, a := range allow {
				if a == host {
					return true
				}
			}
			return false
		},
		Blocked: []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")},
	}, nil)
}

func TestCheck(t *testing.T) {
	s := testService(map[string][]string{
		"hooks.example.com": {"93.184.216.34"},
		"internal.example":  {"10.0.0.5"},
		"mixed.example":     {"93.184.216.34", "192.168.1.1"},
		"rebind.example":    {"169.254.169.254"},
		"node.example":      {"203.0.113.9"},
		"dev.local":         {"127.0.0.1"},
		"v6.example":        {"::ffff:10.1.2.3"},
		"nat64.example":     {"64:ff9b::a9fe:a9fe"},
		"sixto4.example":    {"2002:0a00:0001::1"},
		"public64.example":  {"64:ff9b::5db8:d822"},
	}, "dev.local", "127.0.0.1", "169.254.169.254")
	org := uuid.New()
	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"https://hooks.example.com/in", true},
		{"http://hooks.example.com/in", false},       // plain http needs the allow-list
		{"https://internal.example/x", false},        // RFC 1918
		{"https://mixed.example/x", false},           // any internal address refuses
		{"https://rebind.example/x", false},          // metadata via DNS
		{"http://169.254.169.254/latest", false},     // metadata, even allow-listed
		{"https://[fe80::1]/x", false},               // link-local
		{"https://127.0.0.1/x", true},                // allow-listed loopback
		{"http://dev.local:8080/hook", true},         // allow-listed, plain http
		{"https://localhost/x", false},               // does not resolve here, and not allowed
		{"https://node.example/x", false},            // the nodes' network
		{"https://v6.example/x", false},              // IPv4-mapped private
		{"https://nat64.example/x", false},           // NAT64 of 169.254.169.254
		{"https://[64:ff9b::a9fe:a9fe]/x", false},    // the same, literally
		{"https://sixto4.example/x", false},          // 6to4 of 10.0.0.1
		{"https://public64.example/x", true},         // NAT64 of a public address
		{"ftp://hooks.example.com/x", false},         // scheme
		{"https://user:pw@hooks.example.com", false}, // credentials in the URL
		{"https://[::1]/x", false},
		{"https://10.0.0.1/x", false},
	} {
		_, err := s.Check(context.Background(), org, c.url)
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v, want ok=%v", c.url, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v is not ErrRefused", c.url, err)
		}
	}
	tg, err := s.Check(context.Background(), org, "https://hooks.example.com:8443/in")
	if err != nil || tg.Addr.String() != "93.184.216.34:8443" {
		t.Fatalf("pinned address: %v %v", tg.Addr, err)
	}
}

// TestCheckAddr: a host and port (a project's own SMTP server) passes the
// same rules as a URL's host, and the address returned is the one checked.
func TestCheckAddr(t *testing.T) {
	s := testService(map[string][]string{
		"smtp.example.com": {"93.184.216.34"},
		"internal.example": {"10.0.0.5"},
		"rebind.example":   {"169.254.169.254"},
		"relay.local":      {"192.168.1.20"},
	}, "relay.local", "169.254.169.254")
	org := uuid.New()
	for _, c := range []struct {
		host string
		port int
		ok   bool
	}{
		{"smtp.example.com", 587, true},
		{"internal.example", 587, false},
		{"10.0.0.5", 5432, false},
		{"127.0.0.1", 25, false},
		{"[::1]", 25, false},
		{"rebind.example", 25, false},
		{"169.254.169.254", 80, false}, // metadata, even allow-listed
		{"relay.local", 25, true},      // allow-listed
		{"smtp.example.com", 0, false},
		{"smtp.example.com/x", 25, false},
	} {
		_, err := s.CheckAddr(context.Background(), org, c.host, c.port)
		if (err == nil) != c.ok {
			t.Errorf("%s:%d: err %v, want ok=%v", c.host, c.port, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrRefused) {
			t.Errorf("%s:%d: %v is not ErrRefused", c.host, c.port, err)
		}
	}
	if a, err := s.CheckAddr(context.Background(), org, "SMTP.example.com.", 465); err != nil || a.String() != "93.184.216.34:465" {
		t.Fatalf("pinned address: %v %v", a, err)
	}
}

func TestValidAllowHost(t *testing.T) {
	for h, ok := range map[string]bool{
		"hooks.internal": true, "10.0.0.5": true, "::1": true, "169.254.169.254": false,
		"fe80::1": false, "http://x": false, "x:80": false, "": false,
	} {
		if err := ValidAllowHost(h); (err == nil) != ok {
			t.Errorf("%q: %v, want ok=%v", h, err, ok)
		}
	}
}

func TestSignVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"id":"evt_1"}`)
	h := Sign("whsec_x", now, body)
	if err := Verify("whsec_x", h, body, now.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if Verify("whsec_y", h, body, now, 5*time.Minute) == nil {
		t.Fatal("a wrong secret verified")
	}
	if Verify("whsec_x", h, []byte(`{}`), now, 5*time.Minute) == nil {
		t.Fatal("a changed body verified")
	}
	if Verify("whsec_x", h, body, now.Add(6*time.Minute), 5*time.Minute) == nil {
		t.Fatal("a replay outside the tolerance verified")
	}
}

func TestTake(t *testing.T) {
	now := time.Unix(0, 0)
	s := New(nil, Config{Now: func() time.Time { return now }}, nil)
	org := uuid.New()
	n := 0
	for range 100 {
		if s.Take(org, "webhook", 60, time.Minute) {
			n++
		}
	}
	if n != 60 {
		t.Fatalf("burst: %d, want 60", n)
	}
	now = now.Add(10 * time.Second) // 10 tokens refilled
	n = 0
	for range 100 {
		if s.Take(org, "webhook", 60, time.Minute) {
			n++
		}
	}
	if n != 10 {
		t.Fatalf("after 10s: %d, want 10", n)
	}
	if !s.Take(org, "webhook", 0, time.Minute) {
		t.Fatal("unlimited refused")
	}
}
