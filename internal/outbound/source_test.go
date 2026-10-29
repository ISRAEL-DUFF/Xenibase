package outbound

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/google/uuid"
)

func TestCheckSource(t *testing.T) {
	s := New(nil, Config{
		Resolve: testService(map[string][]string{
			"db.example.com":   {"93.184.216.34"},
			"onprem.example":   {"192.168.1.20"},
			"rebind.example":   {"127.0.0.1"},
			"node.example":     {"93.184.216.40"},
			"internal.example": {"10.0.3.4"},
		}).cfg.Resolve,
		Allowed: func(_ uuid.UUID, host string) bool {
			switch host {
			case "onprem.example", "rebind.example", "node.example", "internal.example", "127.0.0.1", "10.0.0.7":
				return true
			}
			return false
		},
		Internal: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		PlatformAddrs: func(context.Context) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("93.184.216.40")}, nil
		},
	}, nil)
	org := uuid.New()
	for _, c := range []struct {
		host string
		ok   bool
	}{
		{"db.example.com", true},
		{"93.184.216.34", true},
		{"192.168.1.10", false},        // private, not allow-listed
		{"onprem.example", true},       // private, allow-listed by the platform admin
		{"rebind.example", false},      // loopback, even allow-listed
		{"127.0.0.1", false},           // loopback, even allow-listed
		{"[::1]", false},               // loopback
		{"node.example", false},        // a node's own address, even allow-listed
		{"internal.example", false},    // the platform's network, even allow-listed
		{"10.0.0.7", false},            // the platform's network, even allow-listed
		{"169.254.169.254", false},     // metadata
		{"0.0.0.0", false},             // unspecified
		{"/var/run/postgresql", false}, // a socket directory
		{"a.example,b.example", false}, // several hosts
		{"nowhere.example", false},     // does not resolve
	} {
		a, err := s.CheckSource(context.Background(), org, c.host, 5432)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v %v, want ok=%v", c.host, a, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v is not ErrRefused", c.host, err)
		}
	}
	if a, err := s.CheckSource(context.Background(), org, "db.example.com", 5432); err != nil || a != netip.MustParseAddr("93.184.216.34") {
		t.Errorf("checked address %v %v", a, err)
	}
	if _, err := s.CheckSource(context.Background(), org, "db.example.com", 0); err == nil {
		t.Error("port 0 passed")
	}
}
