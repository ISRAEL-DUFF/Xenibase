// Package config loads xenibase-server configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/israel-duff/xenibase/internal/crypto"
)

// Config is the xenibase-server configuration.
type Config struct {
	// ListenAddr is the address the HTTP server binds to.
	ListenAddr string
	// LogLevel is one of debug, info, warn, error.
	LogLevel slog.Level
	// LogFormat is "json" or "text".
	LogFormat string
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration

	// DatabaseURL is the metadata DB connection string.
	DatabaseURL string
	// MasterKey encrypts secrets at rest (spec §7.3). PreviousMasterKeys
	// stay readable during a rotation.
	MasterKey          []byte
	PreviousMasterKeys [][]byte

	// Workers is the number of operations run concurrently.
	Workers int
	// DevEndpoints enables /api/v1/dev/*. Never enable in production.
	DevEndpoints bool

	// Shared is the shared cluster registered at startup (optional).
	Shared SharedCluster
	// Pooler configures the edge poolers; provisioning needs it.
	Pooler Pooler
	// Public is the connection info handed to clients.
	Public Public
	// Web configures cookies, proxies, and setup.
	Web Web
	// PoolerTLS configures the poolers' certificate.
	PoolerTLS PoolerTLS
	// Backups configures agents and backups.
	Backups Backups
	// Insight configures the SQL console and metrics.
	Insight Insight
	// Status connects to the status page (V3 §2.6).
	Status Status
	// Edge configures backend services' gateway (V4 §2.1).
	Edge Edge
	// Payments configures the payment providers (V3 §3.4).
	Payments Payments
	// FreeTier configures pausing and archiving Free projects (V3 §4).
	FreeTier FreeTier
	// Signup configures open signup's protections (V3 §7.4).
	Signup Signup
	// Support configures support's channels (V3 §7.1).
	Support Support
	// AuthPhone is the platform's SMS and WhatsApp for auth codes (V4 §6.2).
	AuthPhone AuthPhone
	// CDN is purged of files that leave a public bucket's URLs (V4 §5.4).
	CDN CDN
	// Cloud configures the provider servers are created with (V3 §5.1).
	Cloud Cloud
	// PGVersions (XENIBASE_PG_VERSIONS, default "17,18") are the Postgres
	// majors projects may run (V3 §2.4); the newest is the default.
	PGVersions []int
}

// Backups configures node agents and backups (M3).
type Backups struct {
	// AgentBootstrapToken lets the install bundle's agent register the
	// local node without an operator (XENIBASE_AGENT_BOOTSTRAP_TOKEN).
	AgentBootstrapToken string
	// Hour is the UTC hour the nightly backup window opens; Jitter spreads
	// projects over it.
	Hour   int
	Jitter time.Duration
	// MetadataURL is the metadata DB as an agent reaches it, for nightly
	// self-backups. Defaults to DatabaseURL; "off" disables them.
	MetadataURL string
	// DedicatedAdminVia is how xenibase-server reaches dedicated instances:
	// "network" (the address agents report; the Compose bundle) or
	// "published" (the port published on the node).
	DedicatedAdminVia string
	// RequireMaintenanceAnnouncement: the maintenance window restarts HA
	// projects only inside maintenance announced 72 hours ahead
	// (XENIBASE_MAINTENANCE_REQUIRE_ANNOUNCEMENT, default true; V3.1 §4.3).
	RequireMaintenanceAnnouncement bool
}

// Load reads configuration from XENIBASE_* environment variables, applying
// defaults for anything unset.
func Load() (Config, error) {
	return load(os.Getenv, os.ReadFile)
}

func load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	cfg := Config{
		ListenAddr:      "127.0.0.1:8080",
		LogLevel:        slog.LevelInfo,
		LogFormat:       "json",
		ShutdownTimeout: 15 * time.Second,
		Workers:         4,
	}
	var errs []error

	if v := getenv("XENIBASE_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := getenv("XENIBASE_LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("XENIBASE_LOG_LEVEL: %w", err))
		}
	}
	if v := getenv("XENIBASE_LOG_FORMAT"); v != "" {
		v = strings.ToLower(v)
		if v != "json" && v != "text" {
			errs = append(errs, fmt.Errorf("XENIBASE_LOG_FORMAT: must be json or text, got %q", v))
		}
		cfg.LogFormat = v
	}
	if v := getenv("XENIBASE_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("XENIBASE_SHUTDOWN_TIMEOUT: %w", err))
		}
		cfg.ShutdownTimeout = d
	}

	cfg.DatabaseURL = getenv("XENIBASE_DATABASE_URL")
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("XENIBASE_DATABASE_URL is required"))
	}

	key, err := loadMasterKey(getenv, readFile)
	if err != nil {
		errs = append(errs, err)
	}
	cfg.MasterKey = key
	if v := getenv("XENIBASE_MASTER_KEY_PREVIOUS"); v != "" {
		for i, s := range strings.Split(v, ",") {
			k, err := crypto.ParseKey(s)
			if err != nil {
				errs = append(errs, fmt.Errorf("XENIBASE_MASTER_KEY_PREVIOUS[%d]: %w", i, err))
				continue
			}
			cfg.PreviousMasterKeys = append(cfg.PreviousMasterKeys, k)
		}
	}

	if v := getenv("XENIBASE_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 256 {
			errs = append(errs, fmt.Errorf("XENIBASE_WORKERS: must be an integer from 1 to 256, got %q", v))
		}
		cfg.Workers = n
	}
	if v := getenv("XENIBASE_DEV_ENDPOINTS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("XENIBASE_DEV_ENDPOINTS: %w", err))
		}
		cfg.DevEndpoints = b
	}

	errs = append(errs, loadProvisioning(getenv, readFile, &cfg)...)
	errs = append(errs, loadWeb(getenv, &cfg)...)
	errs = append(errs, loadPoolerTLS(getenv, &cfg)...)
	errs = append(errs, loadBackups(getenv, &cfg)...)
	errs = append(errs, loadInsight(getenv, &cfg)...)
	errs = append(errs, loadStatus(getenv, readFile, &cfg)...)
	errs = append(errs, loadEdge(getenv, readFile, &cfg)...)
	errs = append(errs, loadPayments(getenv, readFile, &cfg)...)
	errs = append(errs, loadFreeTier(getenv, readFile, &cfg)...)
	errs = append(errs, loadSupport(getenv, readFile, &cfg)...)
	errs = append(errs, loadAuthPhone(getenv, readFile, &cfg)...)
	errs = append(errs, loadCDN(getenv, readFile, &cfg)...)
	errs = append(errs, loadCloud(getenv, readFile, &cfg)...)

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadMasterKey reads XENIBASE_MASTER_KEY, or the file named by
// XENIBASE_MASTER_KEY_FILE (for Docker/systemd secrets). Exactly one is required.
func loadMasterKey(getenv func(string) string, readFile func(string) ([]byte, error)) ([]byte, error) {
	inline, file := getenv("XENIBASE_MASTER_KEY"), getenv("XENIBASE_MASTER_KEY_FILE")
	switch {
	case inline != "" && file != "":
		return nil, errors.New("set only one of XENIBASE_MASTER_KEY and XENIBASE_MASTER_KEY_FILE")
	case inline != "":
		k, err := crypto.ParseKey(inline)
		if err != nil {
			return nil, fmt.Errorf("XENIBASE_MASTER_KEY: %w", err)
		}
		return k, nil
	case file != "":
		b, err := readFile(file)
		if err != nil {
			return nil, fmt.Errorf("XENIBASE_MASTER_KEY_FILE: %w", err)
		}
		k, err := crypto.ParseKey(string(b))
		if err != nil {
			return nil, fmt.Errorf("XENIBASE_MASTER_KEY_FILE: %w", err)
		}
		return k, nil
	default:
		return nil, errors.New("XENIBASE_MASTER_KEY or XENIBASE_MASTER_KEY_FILE is required (generate one with `xenibase-server -gen-master-key`)")
	}
}

func loadBackups(getenv func(string) string, cfg *Config) []error {
	var errs []error
	b := Backups{Hour: 2, Jitter: 2 * time.Hour, AgentBootstrapToken: getenv("XENIBASE_AGENT_BOOTSTRAP_TOKEN"), MetadataURL: cfg.DatabaseURL}
	if t := b.AgentBootstrapToken; t != "" && len(t) < 24 {
		errs = append(errs, errors.New("XENIBASE_AGENT_BOOTSTRAP_TOKEN: must be at least 24 characters"))
	}
	if v := getenv("XENIBASE_BACKUP_HOUR"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 23 {
			errs = append(errs, fmt.Errorf("XENIBASE_BACKUP_HOUR: must be an hour from 0 to 23, got %q", v))
		}
		b.Hour = n
	}
	if v := getenv("XENIBASE_BACKUP_JITTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute || d > 12*time.Hour {
			errs = append(errs, fmt.Errorf("XENIBASE_BACKUP_JITTER: must be a duration from 1m to 12h, got %q", v))
		}
		b.Jitter = d
	}
	b.RequireMaintenanceAnnouncement = true
	switch v := strings.ToLower(getenv("XENIBASE_MAINTENANCE_REQUIRE_ANNOUNCEMENT")); v {
	case "", "true", "1", "yes", "on":
	case "false", "0", "no", "off":
		b.RequireMaintenanceAnnouncement = false
	default:
		errs = append(errs, fmt.Errorf("XENIBASE_MAINTENANCE_REQUIRE_ANNOUNCEMENT: must be true or false, got %q", v))
	}
	switch v := getenv("XENIBASE_DEDICATED_ADMIN_VIA"); v {
	case "", "network", "published":
		b.DedicatedAdminVia = v
	default:
		errs = append(errs, fmt.Errorf("XENIBASE_DEDICATED_ADMIN_VIA: must be network or published, got %q", v))
	}
	switch v := getenv("XENIBASE_METADATA_BACKUP_URL"); v {
	case "":
	case "off":
		b.MetadataURL = ""
	default:
		b.MetadataURL = v
	}
	cfg.Backups = b
	return errs
}
