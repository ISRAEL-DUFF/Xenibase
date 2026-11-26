package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Edge configures backend services' gateway, xenibase-edge (V4 §2.1).
type Edge struct {
	// Domain is the API hostname suffix (XENIBASE_API_DOMAIN): projects are
	// served at https://<ref>.<Domain>. Empty shows no URL.
	Domain string
	// Secret signs xenibase-edge's requests (XENIBASE_EDGE_SECRET or _FILE),
	// the same value as the edges'. Empty disables the edge feed. It serves
	// the regions without their own API domain; an edge for another region
	// needs its own credential (xenibase-server -issue-edge-credential).
	Secret string
	// SecretAllRegions lets Secret serve every region, as before edges had
	// their own credentials (XENIBASE_EDGE_SECRET_ALL_REGIONS; deprecated).
	SecretAllRegions bool
}

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func loadEdge(getenv func(string) string, readFile func(string) ([]byte, error), cfg *Config) []error {
	e := Edge{Domain: strings.ToLower(strings.Trim(getenv("XENIBASE_API_DOMAIN"), "."))}
	var errs []error
	if e.Domain != "" && !domainRe.MatchString(e.Domain) && e.Domain != "localhost" {
		errs = append(errs, fmt.Errorf("XENIBASE_API_DOMAIN: %q is not a domain name", e.Domain))
	}
	secret, file := getenv("XENIBASE_EDGE_SECRET"), getenv("XENIBASE_EDGE_SECRET_FILE")
	switch {
	case secret != "" && file != "":
		errs = append(errs, errors.New("set only one of XENIBASE_EDGE_SECRET and XENIBASE_EDGE_SECRET_FILE"))
	case file != "":
		b, err := readFile(file)
		if err != nil {
			errs = append(errs, fmt.Errorf("XENIBASE_EDGE_SECRET_FILE: %w", err))
		}
		secret = strings.TrimSpace(string(b))
	}
	if secret != "" && len(secret) < 32 {
		errs = append(errs, errors.New("XENIBASE_EDGE_SECRET must be at least 32 characters"))
	}
	e.Secret = secret
	if v := getenv("XENIBASE_EDGE_SECRET_ALL_REGIONS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("XENIBASE_EDGE_SECRET_ALL_REGIONS: %w", err))
		}
		e.SecretAllRegions = b
	}
	cfg.Edge = e
	return errs
}
