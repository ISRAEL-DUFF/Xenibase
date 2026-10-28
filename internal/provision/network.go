package provision

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// MaxAllowedCIDRs is the most networks a project's IP allowlist holds.
const MaxAllowedCIDRs = 50

// NormalizeAllowedCIDRs checks an IP allowlist and returns it canonical,
// sorted and without duplicates: a bare address becomes /32 (or /128),
// and a network with host bits set is refused rather than widened.
func NormalizeAllowedCIDRs(in []string) ([]string, error) {
	if len(in) > MaxAllowedCIDRs {
		return nil, fmt.Errorf("%w: an allowlist holds at most %d networks", ErrInvalid, MaxAllowedCIDRs)
	}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(s, "/") {
			var err error
			if p, err = netip.ParsePrefix(s); err != nil {
				return nil, fmt.Errorf("%w: %q isn't a network in CIDR form (10.0.0.0/16)", ErrInvalid, s)
			}
			if p.Masked() != p {
				return nil, fmt.Errorf("%w: %q has host bits set; did you mean %s?", ErrInvalid, s, p.Masked())
			}
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("%w: %q isn't an IP address or a network", ErrInvalid, s)
			}
			if a.Zone() != "" {
				return nil, fmt.Errorf("%w: %q has a zone; the poolers see addresses without one", ErrInvalid, s)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			// An IPv4 client the way an IPv6 socket writes it.
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		if p.Bits() == 0 {
			return nil, fmt.Errorf("%w: %s allows every address; leave the list empty for that", ErrInvalid, p)
		}
		if !slices.Contains(out, p.String()) {
			out = append(out, p.String())
		}
	}
	slices.Sort(out)
	return out, nil
}

// AllowedFrom reports whether addr is in cidrs (an empty list allows
// every address).
func AllowedFrom(cidrs []string, addr netip.Addr) bool {
	if len(cidrs) == 0 {
		return true
	}
	addr = addr.Unmap()
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil && p.Contains(addr) {
			return true
		}
	}
	return false
}
