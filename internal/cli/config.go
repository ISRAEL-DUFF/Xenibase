package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is ~/.config/xenibase/config.toml: named contexts, so one CLI can
// talk to more than one Xenibase (V2 §7.1).
type Config struct {
	Current  string              `toml:"current"`
	Contexts map[string]*Context `toml:"contexts"`
}

// Context is one server and the organisation its token acts in.
type Context struct {
	Server  string `toml:"server"`
	Org     string `toml:"org,omitempty"`      // organisation id
	OrgName string `toml:"org_name,omitempty"` // for display
	User    string `toml:"user,omitempty"`     // email, for display
	Token   string `toml:"token,omitempty"`
	// TokenCmd prints the token, e.g. "pass show xenibase/home".
	TokenCmd string `toml:"token_cmd,omitempty"`
	TokenID  string `toml:"token_id,omitempty"`
	// CAFile verifies a server whose certificate isn't publicly trusted.
	CAFile string `toml:"ca_file,omitempty"`
}

// configDir is $XENIBASE_CONFIG_DIR, $XDG_CONFIG_HOME/xenibase, or
// ~/.config/xenibase.
func (a *App) configDir() (string, error) {
	if d := a.getenv("XENIBASE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if d := a.getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "xenibase"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "xenibase"), nil
}

func (a *App) configPath() (string, error) {
	d, err := a.configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.toml"), nil
}

func (a *App) loadConfig() (*Config, error) {
	cfg := &Config{Contexts: map[string]*Context{}}
	p, err := a.configPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := toml.Decode(string(b), cfg); err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]*Context{}
	}
	return cfg, nil
}

// saveConfig writes the config readable only by its owner: it holds tokens.
func (a *App) saveConfig(cfg *Config) error {
	p, err := a.configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (c *Config) names() []string {
	out := make([]string, 0, len(c.Contexts))
	for n := range c.Contexts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// target is what a command talks to: a server and a token.
type target struct {
	Name   string // context name ("" from the environment)
	Server string
	Token  string
	Org    string
	CAFile string
}

// resolve picks the server and token: --context, XENIBASE_CONTEXT or the
// current context, with XENIBASE_SERVER or --server, XENIBASE_TOKEN and
// XENIBASE_CA_FILE overriding parts of it (for CI).
//
// A context's token and CA file belong to its server. When the server is
// overridden with a different one, they are dropped: the token comes from
// XENIBASE_TOKEN or from the one saved context for that server, or the
// command fails. Otherwise a token for one Xenibase would be sent to another.
func (a *App) resolve() (target, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return target{}, err
	}
	explicit := true
	name := a.contextName
	if name == "" {
		name = a.getenv("XENIBASE_CONTEXT")
	}
	if name == "" {
		name, explicit = cfg.Current, false
	}
	c, ok := cfg.Contexts[name]
	if !ok {
		if a.contextName != "" {
			return target{}, usageErrorf("no context named %q (see xenibase context list)", a.contextName)
		}
		name, c = "", nil
	}
	envToken, skipped := a.getenv("XENIBASE_TOKEN"), ""
	server := a.server
	if server == "" {
		server = a.getenv("XENIBASE_SERVER")
	}
	if server != "" && (c == nil || !sameServer(server, c.Server)) {
		if c != nil && explicit && envToken == "" {
			return target{}, usageErrorf("context %q is for %s, not %s: its token is not sent to another server (use a context for that server, or set XENIBASE_TOKEN)",
				name, c.Server, server)
		}
		if c != nil {
			skipped = fmt.Sprintf(" (context %q is for %s, and its token is not sent to another server)", name, c.Server)
		}
		name, c = "", nil
		if envToken == "" {
			if name, c, err = cfg.forServer(server); err != nil {
				return target{}, err
			}
		}
	}
	var t target
	if c != nil {
		t = target{Name: name, Server: c.Server, Token: c.Token, Org: c.Org, CAFile: c.CAFile}
	}
	if server != "" {
		t.Server = server
	}
	if envToken != "" {
		t.Token, t.Org = envToken, "" // the token's own org (from whoami)
	} else if t.Token == "" && c != nil && c.TokenCmd != "" {
		out, err := exec.Command("sh", "-c", c.TokenCmd).Output()
		if err != nil {
			return t, fmt.Errorf("token_cmd for context %q: %w", name, err)
		}
		t.Token = strings.TrimSpace(string(out))
	}
	if v := a.getenv("XENIBASE_CA_FILE"); v != "" {
		t.CAFile = v
	}
	if t.Server == "" {
		return t, usageErrorf("not logged in: run xenibase login --server https://… (or set XENIBASE_SERVER and XENIBASE_TOKEN)")
	}
	if t.Token == "" {
		return t, usageErrorf("no token for %s%s: run xenibase login --server %s, or set XENIBASE_TOKEN", t.Server, skipped, t.Server)
	}
	if err := checkServerURL(t.Server); err != nil {
		return t, err
	}
	t.Server = strings.TrimRight(t.Server, "/")
	return t, nil
}

// forServer finds the saved context for server: the only one, or none. Two
// contexts for one server (two organisations) need --context.
func (c *Config) forServer(server string) (string, *Context, error) {
	var found []string
	for _, n := range c.names() {
		if sameServer(server, c.Contexts[n].Server) {
			found = append(found, n)
		}
	}
	switch len(found) {
	case 0:
		return "", nil, nil
	case 1:
		return found[0], c.Contexts[found[0]], nil
	}
	return "", nil, usageErrorf("%d contexts are for %s (%s): choose one with --context", len(found), server, strings.Join(found, ", "))
}

// normalServer is a server URL reduced to what decides where a request goes:
// the scheme and host in lower case, the port (the scheme's default left
// out) and the path without trailing slashes.
func normalServer(s string) string {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimRight(s, "/"))
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/")
}

func sameServer(a, b string) bool { return normalServer(a) == normalServer(b) }

// checkServerURL accepts an https:// URL, or http:// to this machine only:
// a token sent over plain HTTP to another host can be read on the way.
func checkServerURL(server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return usageErrorf("the server must be a URL like https://xenibase.example.com, not %q", server)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return usageErrorf("%s uses plain http://, which would send the token unencrypted: use https:// (http:// is allowed only for localhost, 127.0.0.0/8 and ::1)", server)
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---- Cached personal database credentials ----------------------------------

// creds caches personal database URLs (the password is shown once by the
// server), so connect doesn't rotate them every time.
type credsFile struct {
	Projects map[string]credsEntry `toml:"projects"`
}

type credsEntry struct {
	Server     string `toml:"server"`
	PooledURL  string `toml:"pooled_url"`
	SessionURL string `toml:"session_url"`
}

func (a *App) credsPath() (string, error) {
	d, err := a.configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "credentials.toml"), nil
}

func (a *App) loadCreds() (*credsFile, error) {
	f := &credsFile{Projects: map[string]credsEntry{}}
	p, err := a.credsPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := toml.Decode(string(b), f); err != nil {
		return nil, err
	}
	if f.Projects == nil {
		f.Projects = map[string]credsEntry{}
	}
	return f, nil
}

func (a *App) saveCreds(f *credsFile) error {
	p, err := a.credsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(f); err != nil {
		return err
	}
	return os.WriteFile(p, buf.Bytes(), 0o600)
}
