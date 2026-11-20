package acmedns_test

// Co-hosting (docs/cohosting.md): another app adds its sites and its global
// options to the bundle's Caddy through files in caddy.d. These tests load
// deploy/compose/Caddyfile with Caddy 2.10.0's own Caddyfile adapter and
// provision the result (caddy.Validate: modules loaded and checked, nothing
// listens), with and without such a pair of files, the way `caddy reload`
// inside the container does.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/encode/gzip"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/encode/zstd"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	_ "github.com/caddyserver/caddy/v2/modules/caddytls"
)

// The files the other app writes; the same shape as Taskiem's.
const (
	cohostSites = `app.taskiem.test {
	reverse_proxy taskiem:8080
}

https:// {
	tls {
		on_demand
	}
	reverse_proxy taskiem-public:8082
}

http://*.preview.taskiem.test {
	redir https://{host}{uri} 308
}
`
	cohostGlobal = `on_demand_tls {
	ask http://taskiem-public:9090/tls/ask
}
`
)

// bundleConfig adapts the bundle's Caddyfile with caddy.d in a scratch
// directory holding files (name -> contents), and returns the JSON config.
func bundleConfig(t *testing.T, files map[string]string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "compose", "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src := strings.ReplaceAll(string(raw), "/etc/caddy/caddy.d", filepath.ToSlash(dir))
	// What compose.yaml gives the container.
	t.Setenv("XENIBASE_UI_DOMAIN", "ui.example.test")
	t.Setenv("XENIBASE_ACME_EMAIL", "ops@example.test")
	t.Setenv("CADDY_UI_TLS", "")
	t.Setenv("CADDY_GLOBAL_OPTIONS", "")
	t.Setenv("CADDY_HTTP_REDIRECT", "https://{host}{uri}")
	// Provisioning the TLS app touches Caddy's storage.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ad := caddyconfig.GetAdapter("caddyfile")
	out, _, err := ad.Adapt([]byte(src), map[string]any{"filename": "Caddyfile"})
	if err != nil {
		t.Fatalf("adapt: %v", err)
	}
	return out
}

func validate(cfgJSON []byte) error {
	var cfg caddy.Config
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		return err
	}
	return caddy.Validate(&cfg)
}

func TestBundleCaddyfileAloneIsValid(t *testing.T) {
	out := bundleConfig(t, nil)
	if err := validate(out); err != nil {
		t.Fatalf("the bundle's Caddyfile with an empty caddy.d: %v", err)
	}
	if strings.Contains(string(out), "on_demand") {
		t.Errorf("on-demand TLS is on with nothing in caddy.d: %s", out)
	}
}

func TestBundleCaddyfileTakesAnotherAppsSitesAndGlobalOptions(t *testing.T) {
	out := bundleConfig(t, map[string]string{"taskiem.caddy": cohostSites, "taskiem.global": cohostGlobal})
	if err := validate(out); err != nil {
		t.Fatalf("with taskiem.caddy and taskiem.global: %v", err)
	}
	var cfg struct {
		Apps struct {
			TLS struct {
				Automation struct {
					OnDemand struct {
						Permission struct {
							Module   string `json:"module"`
							Endpoint string `json:"endpoint"`
						} `json:"permission"`
					} `json:"on_demand"`
				} `json:"automation"`
			} `json:"tls"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg.Apps.TLS.Automation.OnDemand.Permission
	if p.Module != "http" || p.Endpoint != "http://taskiem-public:9090/tls/ask" {
		t.Errorf("the global on_demand_tls from taskiem.global didn't arrive: %+v", p)
	}
	for _, want := range []string{`"dial":"taskiem:8080"`, `"dial":"taskiem-public:8082"`, `"dial":"xenibase-server:8080"`, `"app.taskiem.test"`, `"ui.example.test"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the config lacks %s", want)
		}
	}
}

// On-demand TLS without the ask endpoint is refused by Caddy, so the global
// file has to be in place before (or with) the sites file.
func TestOnDemandSitesWithoutTheGlobalFileAreRefused(t *testing.T) {
	out := bundleConfig(t, map[string]string{"taskiem.caddy": cohostSites})
	if err := validate(out); err == nil || !strings.Contains(err.Error(), "permission module") {
		t.Fatalf("on-demand TLS with no ask endpoint: got %v, want Caddy's refusal", err)
	}
}

// With backend services on, enable-services.sh's api.caddy sits next to the
// other app's files (make test-acmedns passes the one it generated).
func TestBundleCaddyfileWithServicesAndAnotherApp(t *testing.T) {
	file := os.Getenv("ACMEDNS_CADDY_FILE")
	if file == "" {
		t.Skip("ACMEDNS_CADDY_FILE not set: run `make test-acmedns`")
	}
	api, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XENIBASE_API_DOMAIN", "api.example.test")
	out := bundleConfig(t, map[string]string{"api.caddy": string(api), "taskiem.caddy": cohostSites, "taskiem.global": cohostGlobal})
	if err := validate(out); err != nil {
		t.Fatalf("api.caddy with taskiem.caddy and taskiem.global: %v", err)
	}
	if !strings.Contains(string(out), `"*.api.example.test"`) {
		t.Errorf("the wildcard API site is missing")
	}
}
