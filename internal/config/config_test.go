package config

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/crypto"
)

var (
	key1 = bytes.Repeat([]byte{1}, crypto.KeySize)
	key2 = bytes.Repeat([]byte{2}, crypto.KeySize)
)

func env(m map[string]string) func(string) string {
	base := map[string]string{
		"XENIBASE_DATABASE_URL": "postgres://localhost/xenibase",
		"XENIBASE_MASTER_KEY":   crypto.EncodeKey(key1),
	}
	for k, v := range m {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func noFiles(string) ([]byte, error) { return nil, errors.New("no such file") }

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(env(nil), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" || cfg.LogLevel != slog.LevelInfo || cfg.LogFormat != "json" ||
		cfg.ShutdownTimeout != 15*time.Second || cfg.Workers != 4 || cfg.DevEndpoints {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if !bytes.Equal(cfg.MasterKey, key1) {
		t.Fatal("master key not loaded")
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"XENIBASE_LISTEN_ADDR":         ":9000",
		"XENIBASE_LOG_LEVEL":           "debug",
		"XENIBASE_LOG_FORMAT":          "TEXT",
		"XENIBASE_SHUTDOWN_TIMEOUT":    "3s",
		"XENIBASE_WORKERS":             "8",
		"XENIBASE_DEV_ENDPOINTS":       "true",
		"XENIBASE_MASTER_KEY_PREVIOUS": crypto.EncodeKey(key2),
	}), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":9000" || cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != "text" ||
		cfg.ShutdownTimeout != 3*time.Second || cfg.Workers != 8 || !cfg.DevEndpoints {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if len(cfg.PreviousMasterKeys) != 1 || !bytes.Equal(cfg.PreviousMasterKeys[0], key2) {
		t.Fatal("previous master key not loaded")
	}
}

func TestMasterKeyFile(t *testing.T) {
	files := func(name string) ([]byte, error) {
		if name == "/run/secrets/key" {
			return []byte(crypto.EncodeKey(key2) + "\n"), nil
		}
		return nil, errors.New("no such file")
	}
	cfg, err := load(env(map[string]string{"XENIBASE_MASTER_KEY": "", "XENIBASE_MASTER_KEY_FILE": "/run/secrets/key"}), files)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cfg.MasterKey, key2) {
		t.Fatal("master key file not loaded")
	}
	if _, err := load(env(map[string]string{"XENIBASE_MASTER_KEY_FILE": "/run/secrets/key"}), files); err == nil {
		t.Fatal("both key sources accepted")
	}
}

func TestLoadInvalid(t *testing.T) {
	for _, m := range []map[string]string{
		{"XENIBASE_LOG_LEVEL": "loud"},
		{"XENIBASE_LOG_FORMAT": "xml"},
		{"XENIBASE_SHUTDOWN_TIMEOUT": "soon"},
		{"XENIBASE_WORKERS": "0"},
		{"XENIBASE_DEV_ENDPOINTS": "maybe"},
		{"XENIBASE_DATABASE_URL": ""},
		{"XENIBASE_MASTER_KEY": ""},
		{"XENIBASE_MASTER_KEY": "c2hvcnQ="},
		{"XENIBASE_MASTER_KEY_PREVIOUS": "nope"},
	} {
		if _, err := load(env(m), noFiles); err == nil {
			t.Errorf("expected error for %v", m)
		}
	}
}

func TestErrorsAreCombined(t *testing.T) {
	_, err := load(func(string) string { return "" }, noFiles)
	if err == nil || !strings.Contains(err.Error(), "XENIBASE_DATABASE_URL") || !strings.Contains(err.Error(), "XENIBASE_MASTER_KEY") {
		t.Fatalf("got %v", err)
	}
}

func TestProvisioningDefaults(t *testing.T) {
	cfg, err := load(env(nil), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pooler.ConfigDir != "" || cfg.Shared.AdminURL != "" {
		t.Fatalf("provisioning should be off by default: %+v %+v", cfg.Pooler, cfg.Shared)
	}
	if cfg.Public != (Public{Host: "localhost", SessionPort: 5432, PooledPort: 6543, SSLMode: "require"}) {
		t.Fatalf("public: %+v", cfg.Public)
	}
}

func TestProvisioningConfig(t *testing.T) {
	cfg, err := load(env(map[string]string{
		"XENIBASE_DB_HOST":               "db.example.com",
		"XENIBASE_DB_SESSION_PORT":       "6432",
		"XENIBASE_DB_SSLMODE":            "disable",
		"XENIBASE_POOLER_CONFIG_DIR":     "/var/lib/xenibase/pooler",
		"XENIBASE_POOLER_FILE_MODE":      "644",
		"XENIBASE_POOLER_ADMIN_PASSWORD": "pw",
		"XENIBASE_POOLER_POOLED_ADDR":    "10.0.0.2:6543",
		"XENIBASE_SHARED_ADMIN_URL":      "postgres://xenibase_admin:x@10.0.0.3/postgres",
		"XENIBASE_SHARED_POOLER_PORT":    "5433",
		"XENIBASE_POOLER_ADMIN_ALLOW":    "10.0.0.0/16, fd00::/8",
	}), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Pooler
	if p.FileMode != 0o644 || p.AdminUser != "xenibase" || p.AdminPassword != "pw" ||
		p.SessionAddr != "db.example.com:6432" || p.PooledAddr != "10.0.0.2:6543" || p.SSLMode != "prefer" {
		t.Fatalf("pooler: %+v", p)
	}
	if len(p.AdminAllow) != 2 || p.AdminAllow[0] != "10.0.0.0/16" || p.AdminAllow[1] != "fd00::/8" {
		t.Fatalf("admin allow: %v", p.AdminAllow)
	}
	if cfg.Shared.NodeName != "local" || cfg.Shared.PoolerPort != 5433 {
		t.Fatalf("shared: %+v", cfg.Shared)
	}
}

func TestProvisioningInvalid(t *testing.T) {
	for _, m := range []map[string]string{
		{"XENIBASE_POOLER_CONFIG_DIR": "/x"}, // no admin password
		{"XENIBASE_POOLER_CONFIG_DIR": "/x", "XENIBASE_POOLER_ADMIN_PASSWORD": "p", "XENIBASE_POOLER_FILE_MODE": "9"},
		{"XENIBASE_POOLER_CONFIG_DIR": "/x", "XENIBASE_POOLER_ADMIN_PASSWORD": "p", "XENIBASE_POOLER_SESSION_ADDR": "nohost"},
		{"XENIBASE_POOLER_CONFIG_DIR": "/x", "XENIBASE_POOLER_ADMIN_PASSWORD": "p", "XENIBASE_POOLER_ADMIN_ALLOW": "10.0.0.1/16"},
		{"XENIBASE_DB_SSLMODE": "sometimes"},
		{"XENIBASE_DB_POOLED_PORT": "99999"},
	} {
		if _, err := load(env(m), noFiles); err == nil {
			t.Errorf("expected error for %v", m)
		}
	}
}

func TestWebConfig(t *testing.T) {
	cfg, err := load(env(nil), noFiles)
	if err != nil || !cfg.Web.SecureCookies || len(cfg.Web.TrustedProxies) != 0 {
		t.Fatalf("defaults: %+v %v", cfg.Web, err)
	}
	cfg, err = load(env(map[string]string{
		"XENIBASE_COOKIE_SECURE":   "false",
		"XENIBASE_TRUSTED_PROXIES": "172.16.0.0/12, 127.0.0.1",
		"XENIBASE_PUBLIC_IPS":      "203.0.113.7",
	}), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.SecureCookies || len(cfg.Web.TrustedProxies) != 2 || cfg.Web.TrustedProxies[1].String() != "127.0.0.1/32" ||
		len(cfg.Web.PublicIPs) != 1 {
		t.Fatalf("web: %+v", cfg.Web)
	}
	for _, m := range []map[string]string{{"XENIBASE_TRUSTED_PROXIES": "nope"}, {"XENIBASE_PUBLIC_IPS": "x"}, {"XENIBASE_COOKIE_SECURE": "kinda"}} {
		if _, err := load(env(m), noFiles); err == nil {
			t.Errorf("accepted %v", m)
		}
	}
}

func TestInsightConfig(t *testing.T) {
	cfg, err := load(env(nil), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Insight.ConsoleDisabled || cfg.Insight.MetricsInterval != time.Minute || cfg.Insight.MetricsToken != "" {
		t.Fatalf("insight defaults: %+v", cfg.Insight)
	}
	cfg, err = load(env(map[string]string{
		"XENIBASE_CONSOLE_DISABLED": "true", "XENIBASE_METRICS_INTERVAL": "15s", "XENIBASE_METRICS_TOKEN": strings.Repeat("t", 24),
	}), noFiles)
	if err != nil || !cfg.Insight.ConsoleDisabled || cfg.Insight.MetricsInterval != 15*time.Second || len(cfg.Insight.MetricsToken) != 24 {
		t.Fatalf("insight overrides: %+v %v", cfg.Insight, err)
	}
	for k, v := range map[string]string{
		"XENIBASE_CONSOLE_DISABLED": "maybe",
		"XENIBASE_METRICS_INTERVAL": "100ms",
		"XENIBASE_METRICS_TOKEN":    "short",
	} {
		if _, err := load(env(map[string]string{k: v}), noFiles); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: %v", k, v, err)
		}
	}
}

func TestStatusConfig(t *testing.T) {
	cfg, err := load(env(nil), noFiles)
	if err != nil || cfg.Status.URL != "" {
		t.Fatalf("status defaults: %+v %v", cfg.Status, err)
	}
	secret := strings.Repeat("s", 32)
	cfg, err = load(env(map[string]string{
		"XENIBASE_STATUS_URL": "https://status.example.com/", "XENIBASE_STATUS_PUSH_SECRET": secret,
		"XENIBASE_STATUS_COMPONENTS": "dashboard, edge-pooler,,backups", "XENIBASE_STATUS_REGION": "eu-central",
	}), noFiles)
	if err != nil || cfg.Status.URL != "https://status.example.com" || cfg.Status.PushSecret != secret ||
		strings.Join(cfg.Status.Components, "|") != "dashboard|edge-pooler|backups" || cfg.Status.Region != "eu-central" {
		t.Fatalf("status: %+v %v", cfg.Status, err)
	}
	for name, m := range map[string]map[string]string{
		"no secret":    {"XENIBASE_STATUS_URL": "https://status.example.com"},
		"short secret": {"XENIBASE_STATUS_URL": "https://status.example.com", "XENIBASE_STATUS_PUSH_SECRET": "short"},
		"bad url":      {"XENIBASE_STATUS_URL": "status.example.com", "XENIBASE_STATUS_PUSH_SECRET": secret},
		"both":         {"XENIBASE_STATUS_PUSH_SECRET": secret, "XENIBASE_STATUS_PUSH_SECRET_FILE": "/x"},
	} {
		if _, err := load(env(m), noFiles); err == nil || !strings.Contains(err.Error(), "XENIBASE_STATUS") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
