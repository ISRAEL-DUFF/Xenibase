package provision

import (
	"net/netip"
	"slices"
	"testing"
)

func TestNormalizeAllowedCIDRs(t *testing.T) {
	got, err := NormalizeAllowedCIDRs([]string{" 102.89.10.4 ", "10.0.0.0/16", "10.0.0.0/16", "2001:db8::/32", "::ffff:192.0.2.1", ""})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.0.0.0/16", "102.89.10.4/32", "192.0.2.1/32", "2001:db8::/32"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"10.0.0.5/16", "0.0.0.0/0", "::/0", "nope", "10.0.0.0/33", "fe80::1%eth0"} {
		if _, err := NormalizeAllowedCIDRs([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	many := make([]string, MaxAllowedCIDRs+1)
	for i := range many {
		many[i] = netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}).String()
	}
	if _, err := NormalizeAllowedCIDRs(many); err == nil {
		t.Error("51 networks accepted")
	}
	if !AllowedFrom(nil, netip.MustParseAddr("1.2.3.4")) || !AllowedFrom(got, netip.MustParseAddr("10.0.7.1")) || AllowedFrom(got, netip.MustParseAddr("10.1.0.1")) {
		t.Error("AllowedFrom")
	}
}
