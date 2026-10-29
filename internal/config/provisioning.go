package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// SharedCluster is a shared-tier cluster registered at startup. Until node
// agents arrive (M4), this is how the first node is added.
type SharedCluster struct {
	// AdminURL (XENIBASE_SHARED_ADMIN_URL) connects as the cluster superuser
	// xenibase_admin. Empty disables registration.
	AdminURL string
	// NodeName (XENIBASE_SHARED_NODE_NAME, default "local").
	NodeName string
	// PoolerHost/PoolerPort (XENIBASE_SHARED_POOLER_HOST/_PORT) are how the
	// poolers reach the cluster; default to AdminURL's host and port.
	PoolerHost string
	PoolerPort int
	// NodeRole (XENIBASE_SHARED_NODE_ROLE, default "both") is the node's role
	// when first registered; later it is changed in the UI.
	NodeRole string
}

// Pooler configures xenibase-server's control of the edge PgBouncers.
type Pooler struct {
	// ConfigDir (XENIBASE_POOLER_CONFIG_DIR) receives databases.ini and
	// userlist.txt. Empty disables provisioning.
	ConfigDir string
	// FileMode (XENIBASE_POOLER_FILE_MODE, octal, default 0640).
	FileMode os.FileMode
	// SessionAddr and PooledAddr (XENIBASE_POOLER_SESSION_ADDR/_POOLED_ADDR)
	// are host:port of the two poolers as xenibase-server reaches them, for
	// the admin console and smoke tests. Default to the public address.
	SessionAddr string
	PooledAddr  string
	// AdminUser/AdminPassword (XENIBASE_POOLER_ADMIN_USER, default "xenibase";
	// XENIBASE_POOLER_ADMIN_PASSWORD or _FILE) log in to the admin console.
	AdminUser     string
	AdminPassword string
	// SSLMode for admin and smoke-test connections (XENIBASE_POOLER_SSLMODE,
	// default "prefer").
	SSLMode string

	// Local (XENIBASE_POOLER_LOCAL, default true): SessionAddr and PooledAddr
	// are poolers xenibase-server administers itself, next to it, reading
	// ConfigDir. With a standby pooler pair (V3 §2.1) set it to false and
	// point SessionAddr/PooledAddr at the floating IP for smoke tests; the
	// pooler hosts are then administered through their nodes.
	Local bool
	// HostSessionPort and HostPooledPort (XENIBASE_POOLER_HOST_SESSION_PORT,
	// _POOLED_PORT, default 5432 and 6543) are the admin consoles on each
	// pooler host's private address.
	HostSessionPort int
	HostPooledPort  int
	// FloatingIP is the edge pooler's floating IP, when there is one.
	FloatingIP FloatingIP
	// AdminAllow (XENIBASE_POOLER_ADMIN_ALLOW, comma-separated CIDRs) are
	// the networks the poolers' admin consoles accept logins from. Empty:
	// loopback and the private ranges (pooler.DefaultAdminNetworks).
	AdminAllow []string
}

// FloatingIP configures the Hetzner floating IP in front of the pooler
// hosts (V3 §2.1), which xenibase-server checks and corrects.
type FloatingIP struct {
	// ID (XENIBASE_FLOATING_IP_ID); empty means no floating IP to manage.
	ID string
	// Token (XENIBASE_HETZNER_TOKEN or _FILE) and API (XENIBASE_HETZNER_API,
	// default the Hetzner Cloud API).
	Token string
	API   string
}

// Public is the connection info clients receive (spec §5.1).
type Public struct {
	// Host (XENIBASE_DB_HOST, default "localhost"), e.g. db.example.com.
	Host string
	// SessionPort (XENIBASE_DB_SESSION_PORT, default 5432) and PooledPort
	// (XENIBASE_DB_POOLED_PORT, default 6543).
	SessionPort int
	PooledPort  int
	// SSLMode (XENIBASE_DB_SSLMODE, default "require").
	SSLMode string
}

var sslModes = map[string]bool{"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true}

func loadProvisioning(getenv func(string) string, readFile func(string) ([]byte, error), cfg *Config) []error {
	var errs []error
	port := func(name string, def int) int {
		v := getenv(name)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("%s: must be a port number, got %q", name, v))
		}
		return n
	}
	str := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}
	sslmode := func(name, def string) string {
		v := str(name, def)
		if !sslModes[v] {
			errs = append(errs, fmt.Errorf("%s: unknown sslmode %q", name, v))
		}
		return v
	}
	addr := func(name, def string) string {
		v := str(name, def)
		if _, _, err := net.SplitHostPort(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: must be host:port, got %q", name, v))
		}
		return v
	}

	cfg.Public = Public{
		Host:        str("XENIBASE_DB_HOST", "localhost"),
		SessionPort: port("XENIBASE_DB_SESSION_PORT", 5432),
		PooledPort:  port("XENIBASE_DB_POOLED_PORT", 6543),
		SSLMode:     sslmode("XENIBASE_DB_SSLMODE", "require"),
	}
	cfg.PGVersions = []int{17, 18}
	if v := getenv("XENIBASE_PG_VERSIONS"); v != "" {
		cfg.PGVersions = nil
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 13 || n > 99 {
				errs = append(errs, fmt.Errorf("XENIBASE_PG_VERSIONS: %q is not a Postgres major version", f))
				continue
			}
			cfg.PGVersions = append(cfg.PGVersions, n)
		}
	}

	cfg.Shared = SharedCluster{
		AdminURL:   getenv("XENIBASE_SHARED_ADMIN_URL"),
		NodeName:   str("XENIBASE_SHARED_NODE_NAME", "local"),
		PoolerHost: getenv("XENIBASE_SHARED_POOLER_HOST"),
		PoolerPort: port("XENIBASE_SHARED_POOLER_PORT", 0),
		NodeRole:   str("XENIBASE_SHARED_NODE_ROLE", "both"),
	}
	if r := cfg.Shared.NodeRole; r != "shared" && r != "dedicated" && r != "both" {
		errs = append(errs, fmt.Errorf("XENIBASE_SHARED_NODE_ROLE: must be shared, dedicated, or both, got %q", r))
	}

	p := Pooler{ConfigDir: getenv("XENIBASE_POOLER_CONFIG_DIR"), FileMode: 0o640}
	if p.ConfigDir == "" {
		cfg.Pooler = p
		return errs
	}
	if v := getenv("XENIBASE_POOLER_FILE_MODE"); v != "" {
		m, err := strconv.ParseUint(v, 8, 32)
		if err != nil || m > 0o777 || m&0o600 != 0o600 {
			errs = append(errs, fmt.Errorf("XENIBASE_POOLER_FILE_MODE: octal mode readable and writable by the owner, got %q", v))
		}
		p.FileMode = os.FileMode(m)
	}
	pub := cfg.Public
	p.SessionAddr = addr("XENIBASE_POOLER_SESSION_ADDR", net.JoinHostPort(pub.Host, strconv.Itoa(pub.SessionPort)))
	p.PooledAddr = addr("XENIBASE_POOLER_POOLED_ADDR", net.JoinHostPort(pub.Host, strconv.Itoa(pub.PooledPort)))
	p.AdminUser = str("XENIBASE_POOLER_ADMIN_USER", "xenibase")
	p.SSLMode = sslmode("XENIBASE_POOLER_SSLMODE", "prefer")

	inline, file := getenv("XENIBASE_POOLER_ADMIN_PASSWORD"), getenv("XENIBASE_POOLER_ADMIN_PASSWORD_FILE")
	switch {
	case inline != "" && file != "":
		errs = append(errs, errors.New("set only one of XENIBASE_POOLER_ADMIN_PASSWORD and XENIBASE_POOLER_ADMIN_PASSWORD_FILE"))
	case inline != "":
		p.AdminPassword = inline
	case file != "":
		b, err := readFile(file)
		if err != nil {
			errs = append(errs, fmt.Errorf("XENIBASE_POOLER_ADMIN_PASSWORD_FILE: %w", err))
		}
		p.AdminPassword = strings.TrimRight(string(b), "\r\n")
	default:
		errs = append(errs, errors.New("XENIBASE_POOLER_ADMIN_PASSWORD or _FILE is required with XENIBASE_POOLER_CONFIG_DIR"))
	}
	for _, c := range strings.Split(getenv("XENIBASE_POOLER_ADMIN_ALLOW"), ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		pfx, err := netip.ParsePrefix(c)
		if err != nil || pfx.Masked() != pfx {
			errs = append(errs, fmt.Errorf("XENIBASE_POOLER_ADMIN_ALLOW: %q is not a network such as 10.0.0.0/16", c))
			continue
		}
		p.AdminAllow = append(p.AdminAllow, pfx.String())
	}
	p.Local = getenv("XENIBASE_POOLER_LOCAL") != "false"
	p.HostSessionPort = port("XENIBASE_POOLER_HOST_SESSION_PORT", 5432)
	p.HostPooledPort = port("XENIBASE_POOLER_HOST_POOLED_PORT", 6543)
	p.FloatingIP = FloatingIP{ID: getenv("XENIBASE_FLOATING_IP_ID"), API: getenv("XENIBASE_HETZNER_API")}
	if p.FloatingIP.ID != "" {
		tok, tokFile := getenv("XENIBASE_HETZNER_TOKEN"), getenv("XENIBASE_HETZNER_TOKEN_FILE")
		switch {
		case tok != "" && tokFile != "":
			errs = append(errs, errors.New("set only one of XENIBASE_HETZNER_TOKEN and XENIBASE_HETZNER_TOKEN_FILE"))
		case tok != "":
			p.FloatingIP.Token = tok
		case tokFile != "":
			b, err := readFile(tokFile)
			if err != nil {
				errs = append(errs, fmt.Errorf("XENIBASE_HETZNER_TOKEN_FILE: %w", err))
			}
			p.FloatingIP.Token = strings.TrimSpace(string(b))
		default:
			errs = append(errs, errors.New("XENIBASE_HETZNER_TOKEN or _FILE is required with XENIBASE_FLOATING_IP_ID"))
		}
	}
	cfg.Pooler = p
	return errs
}

// PoolerTLS configures the poolers' client-facing certificate.
type PoolerTLS struct {
	// Mode (XENIBASE_POOLER_TLS): self-signed (default), acme, files, or off.
	Mode string
	// DataDir (XENIBASE_DATA_DIR, default /var/lib/xenibase) holds ACME state.
	DataDir string
	// ACMEEmail (XENIBASE_ACME_EMAIL), ACMECA (XENIBASE_ACME_CA, default Let's
	// Encrypt), and ACMECARoots (XENIBASE_ACME_CA_ROOTS, a PEM file trusted for
	// the CA's own HTTPS, for private or test CAs).
	ACMEEmail   string
	ACMECA      string
	ACMECARoots string
	// CertFile/KeyFile (XENIBASE_POOLER_TLS_CERT/_KEY) for mode files.
	CertFile string
	KeyFile  string
}

func loadPoolerTLS(getenv func(string) string, cfg *Config) []error {
	t := PoolerTLS{
		Mode:        getenv("XENIBASE_POOLER_TLS"),
		DataDir:     getenv("XENIBASE_DATA_DIR"),
		ACMEEmail:   getenv("XENIBASE_ACME_EMAIL"),
		ACMECA:      getenv("XENIBASE_ACME_CA"),
		ACMECARoots: getenv("XENIBASE_ACME_CA_ROOTS"),
		CertFile:    getenv("XENIBASE_POOLER_TLS_CERT"),
		KeyFile:     getenv("XENIBASE_POOLER_TLS_KEY"),
	}
	if t.Mode == "" {
		t.Mode = "self-signed"
	}
	if t.DataDir == "" {
		t.DataDir = "/var/lib/xenibase"
	}
	var errs []error
	switch t.Mode {
	case "self-signed", "off":
	case "acme":
		if t.ACMEEmail == "" {
			errs = append(errs, fmt.Errorf("XENIBASE_ACME_EMAIL is required with XENIBASE_POOLER_TLS=acme"))
		}
	case "files":
		if t.CertFile == "" || t.KeyFile == "" {
			errs = append(errs, fmt.Errorf("XENIBASE_POOLER_TLS_CERT and _KEY are required with XENIBASE_POOLER_TLS=files"))
		}
	default:
		errs = append(errs, fmt.Errorf("XENIBASE_POOLER_TLS: want self-signed, acme, files, or off; got %q", t.Mode))
	}
	cfg.PoolerTLS = t
	return errs
}
