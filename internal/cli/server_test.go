package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeServer answers GET /api/v1/me and records the bearer tokens it was sent.
type fakeServer struct {
	*httptest.Server
	mu     sync.Mutex
	tokens []string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokens = append(f.tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"someone@example.com"}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

func runWith(t *testing.T, env map[string]string, cfg *Config, args ...string) (int, string) {
	t.Helper()
	env["XENIBASE_CONFIG_DIR"] = t.TempDir()
	var out bytes.Buffer
	a := &App{Stdout: &out, Stderr: &out, Stdin: strings.NewReader(""), Getenv: func(k string) string { return env[k] }}
	if cfg != nil {
		if err := a.saveConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	return a.Run(args), out.String()
}

func prodConfig() *Config {
	return &Config{Current: "prod", Contexts: map[string]*Context{
		"prod": {Server: "https://prod.example", Token: "xb_prod_secret", CAFile: "/nonexistent/prod-ca.pem"},
	}}
}

// A context's token goes only to its own server: --server or XENIBASE_SERVER
// naming another one must not carry it there.
func TestServerOverrideDoesNotSendTheContextsToken(t *testing.T) {
	srv := newFakeServer(t)

	code, out := runWith(t, map[string]string{}, prodConfig(), "whoami", "--server", srv.URL)
	if code != ExitUsage || !strings.Contains(out, `context "prod" is for https://prod.example`) {
		t.Fatalf("--server: exit %d: %s", code, out)
	}
	code, out = runWith(t, map[string]string{"XENIBASE_SERVER": srv.URL}, prodConfig(), "whoami")
	if code != ExitUsage {
		t.Fatalf("XENIBASE_SERVER: exit %d: %s", code, out)
	}
	if got := srv.sent(); len(got) != 0 {
		t.Fatalf("the server was sent %q", got)
	}

	// XENIBASE_TOKEN is meant for the overriding server.
	code, out = runWith(t, map[string]string{"XENIBASE_SERVER": srv.URL, "XENIBASE_TOKEN": "xb_env"}, prodConfig(), "whoami")
	if code != ExitOK {
		t.Fatalf("XENIBASE_TOKEN: exit %d: %s", code, out)
	}
	// A saved context for the overriding server supplies its own token.
	cfg := prodConfig()
	cfg.Contexts["local"] = &Context{Server: srv.URL + "/", Token: "xb_local"}
	if code, out = runWith(t, map[string]string{}, cfg, "whoami", "--server", srv.URL); code != ExitOK {
		t.Fatalf("matching context: exit %d: %s", code, out)
	}
	if got := strings.Join(srv.sent(), ","); got != "xb_env,xb_local" {
		t.Fatalf("tokens sent: %s", got)
	}

	// Naming the context and another server is a contradiction.
	if code, out = runWith(t, map[string]string{}, cfg, "whoami", "--context", "prod", "--server", srv.URL); code != ExitUsage {
		t.Fatalf("--context prod --server other: exit %d: %s", code, out)
	}
	// Two contexts for the server: say which.
	cfg.Contexts["local2"] = &Context{Server: srv.URL, Token: "xb_local2"}
	if code, out = runWith(t, map[string]string{}, cfg, "whoami", "--server", srv.URL); code != ExitUsage || !strings.Contains(out, "--context") {
		t.Fatalf("two matching contexts: exit %d: %s", code, out)
	}
	if code, out = runWith(t, map[string]string{}, cfg, "whoami", "--context", "local2", "--server", srv.URL); code != ExitOK {
		t.Fatalf("--context local2: exit %d: %s", code, out)
	}
	if got := srv.sent(); got[len(got)-1] != "xb_local2" || strings.Contains(strings.Join(got, ","), "xb_prod_secret") {
		t.Fatalf("tokens sent: %q", got)
	}
}

func TestResolveKeepsTheContextForTheSameServer(t *testing.T) {
	env := map[string]string{"XENIBASE_CONFIG_DIR": t.TempDir()}
	a := &App{Getenv: func(k string) string { return env[k] }}
	if err := a.saveConfig(prodConfig()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"https://prod.example/", "HTTPS://Prod.Example:443", "https://prod.example//"} {
		a.server = s
		tg, err := a.resolve()
		if err != nil || tg.Token != "xb_prod_secret" || tg.CAFile != "/nonexistent/prod-ca.pem" || tg.Name != "prod" {
			t.Errorf("%s: %+v %v", s, tg, err)
		}
	}
	for _, s := range []string{"https://prod.example:8443", "http://prod.example", "https://prod.example.evil", "https://prod.example/other"} {
		a.server = s
		env["XENIBASE_TOKEN"] = "xb_env"
		tg, err := a.resolve()
		delete(env, "XENIBASE_TOKEN")
		if s == "http://prod.example" {
			if exitCode(err) != ExitUsage {
				t.Errorf("%s: %+v %v", s, tg, err)
			}
			continue
		}
		if err != nil || tg.Token != "xb_env" || tg.CAFile != "" || tg.Name != "" {
			t.Errorf("%s: %+v %v", s, tg, err)
		}
	}
}

func TestPlainHTTPOnlyToLoopback(t *testing.T) {
	for s, ok := range map[string]bool{
		"https://xenibase.example.com": true,
		"http://localhost:8080":        true,
		"http://LOCALHOST":             true,
		"http://127.0.0.1:4000":        true,
		"http://127.8.9.10":            true,
		"http://[::1]:8080":            true,
		"http://xenibase.example.com":  false,
		"http://10.0.0.1:8080":         false,
		"http://localhost.example.com": false,
		"http://[::2]":                 false,
		"ftp://xenibase.example.com":   false,
		"xenibase.example.com":         false,
	} {
		if err := checkServerURL(s); (err == nil) != ok {
			t.Errorf("%s: %v", s, err)
		}
	}

	// login refuses before sending anything.
	code, out := runWith(t, map[string]string{}, nil, "login", "--server", "http://xenibase.example.com", "--token", "xb_x")
	if code != ExitUsage || !strings.Contains(out, "http://") {
		t.Fatalf("login over http: exit %d: %s", code, out)
	}
	// A saved token is not used over http to another host either.
	cfg := &Config{Current: "old", Contexts: map[string]*Context{"old": {Server: "http://xenibase.example.com", Token: "xb_old"}}}
	if code, out = runWith(t, map[string]string{}, cfg, "whoami"); code != ExitUsage {
		t.Fatalf("stored http context: exit %d: %s", code, out)
	}
	// Loopback is fine (the test server is http://127.0.0.1).
	srv := newFakeServer(t)
	cfg = &Config{Current: "dev", Contexts: map[string]*Context{"dev": {Server: srv.URL, Token: "xb_dev"}}}
	if code, out = runWith(t, map[string]string{}, cfg, "whoami"); code != ExitOK {
		t.Fatalf("loopback http: exit %d: %s", code, out)
	}
}
