package config

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// FreeTier configures pausing and archiving Free projects (V3 §4) and the
// waker that answers their connections.
type FreeTier struct {
	// WakerAddr is the waker as the poolers reach it, host:port
	// (XENIBASE_WAKER_ADDR). Empty turns pausing off: a paused project's
	// clients would only be refused.
	WakerAddr string
	// WakerListen is where the waker listens (XENIBASE_WAKER_LISTEN, default
	// ":6435"). Only the poolers should reach it.
	WakerListen string
	// WakerAllow (XENIBASE_WAKER_ALLOW, comma-separated CIDRs) are further
	// networks the waker accepts connections from, besides loopback and
	// the poolers' own addresses.
	WakerAllow []netip.Prefix
	// PauseAfter, ArchiveAfter and DeleteAfter are XENIBASE_FREE_PAUSE_AFTER
	// (168h), XENIBASE_FREE_ARCHIVE_AFTER (2160h, 90 days) and
	// XENIBASE_FREE_DELETE_AFTER (8760h, a year).
	PauseAfter   time.Duration
	ArchiveAfter time.Duration
	DeleteAfter  time.Duration
}

// WakerHostPort splits WakerAddr.
func (f FreeTier) WakerHostPort() (string, int) {
	h, p, err := net.SplitHostPort(f.WakerAddr)
	if err != nil {
		return "", 0
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

// Signup configures open signup's protections (V3 §7.4).
type Signup struct {
	// TurnstileSiteKey and TurnstileSecret are the Cloudflare Turnstile
	// keys (XENIBASE_TURNSTILE_SITE_KEY, XENIBASE_TURNSTILE_SECRET or _FILE);
	// with them set, signing up needs a solved challenge.
	TurnstileSiteKey string
	TurnstileSecret  string
	// TurnstileURL is the verification endpoint (XENIBASE_TURNSTILE_URL).
	TurnstileURL string
	// PerIP is how many accounts one IP address may create a day
	// (XENIBASE_SIGNUPS_PER_IP, default 3; 0 turns the limit off).
	PerIP int
}

func loadFreeTier(getenv func(string) string, readFile func(string) ([]byte, error), cfg *Config) []error {
	var errs []error
	f := FreeTier{WakerAddr: getenv("XENIBASE_WAKER_ADDR"), WakerListen: getenv("XENIBASE_WAKER_LISTEN"),
		PauseAfter: 7 * 24 * time.Hour, ArchiveAfter: 90 * 24 * time.Hour, DeleteAfter: 365 * 24 * time.Hour}
	if f.WakerListen == "" {
		f.WakerListen = ":6435"
	}
	for _, c := range strings.Split(getenv("XENIBASE_WAKER_ALLOW"), ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		pfx, err := netip.ParsePrefix(c)
		if err != nil || pfx.Masked() != pfx {
			errs = append(errs, fmt.Errorf("XENIBASE_WAKER_ALLOW: %q is not a network such as 10.0.0.0/16", c))
			continue
		}
		f.WakerAllow = append(f.WakerAllow, pfx)
	}
	if f.WakerAddr != "" {
		if h, p := f.WakerHostPort(); h == "" || p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("XENIBASE_WAKER_ADDR %q: want host:port", f.WakerAddr))
		}
	}
	for name, dst := range map[string]*time.Duration{
		"XENIBASE_FREE_PAUSE_AFTER": &f.PauseAfter, "XENIBASE_FREE_ARCHIVE_AFTER": &f.ArchiveAfter, "XENIBASE_FREE_DELETE_AFTER": &f.DeleteAfter,
	} {
		if v := getenv(name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 2*time.Hour {
				errs = append(errs, fmt.Errorf("%s %q: want a duration of at least 2h", name, v))
				continue
			}
			*dst = d
		}
	}
	cfg.FreeTier = f

	s := Signup{TurnstileSiteKey: getenv("XENIBASE_TURNSTILE_SITE_KEY"), TurnstileURL: getenv("XENIBASE_TURNSTILE_URL"), PerIP: 3}
	secret, err := secretFrom(getenv, readFile, "XENIBASE_TURNSTILE_SECRET")
	if err != nil {
		errs = append(errs, err)
	}
	s.TurnstileSecret = secret
	if (s.TurnstileSiteKey == "") != (s.TurnstileSecret == "") {
		errs = append(errs, fmt.Errorf("set both XENIBASE_TURNSTILE_SITE_KEY and XENIBASE_TURNSTILE_SECRET, or neither"))
	}
	if v := getenv("XENIBASE_SIGNUPS_PER_IP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("XENIBASE_SIGNUPS_PER_IP %q: want a whole number", v))
		} else {
			s.PerIP = n
		}
	}
	cfg.Signup = s
	return errs
}
