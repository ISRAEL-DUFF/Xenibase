package load

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/totp"
)

// control is the management API the load tests drive: the in-process test
// environment, or a real install (remoteControl).
type control interface {
	Do(method, path string, body, out any) int
	WaitOperation(id uuid.UUID) gen.Operation
	CreateOrg(name string) uuid.UUID
}

// remoteTarget is a real install named by XENIBASE_LOAD_TARGET (V4.1 §13):
// xenibase-server's URL, a platform admin's sign-in, and how to reach the
// edge. deploy/loadtest/README.md sets one up.
type remoteTarget struct {
	URL, Email, Password, TOTPSecret string
	// APIDomain is XENIBASE_API_DOMAIN: projects are https://<ref>.<domain>.
	APIDomain string
	// EdgeAddr, when set, is dialled for every project host instead of
	// DNS (no wildcard record needed); TLS still checks the project's name.
	EdgeAddr string
	// EdgeCA is a PEM file the edge's certificate chains to, for a
	// self-signed wildcard; empty uses the system roots.
	EdgeCA string
}

// remoteFromEnv reads the target, or returns nil to run in-process.
func remoteFromEnv(t *testing.T) *remoteTarget {
	url := os.Getenv("XENIBASE_LOAD_TARGET")
	if url == "" {
		return nil
	}
	r := &remoteTarget{URL: url, Email: os.Getenv("XENIBASE_LOAD_EMAIL"), Password: os.Getenv("XENIBASE_LOAD_PASSWORD"),
		TOTPSecret: os.Getenv("XENIBASE_LOAD_TOTP_SECRET"), APIDomain: os.Getenv("XENIBASE_LOAD_API_DOMAIN"),
		EdgeAddr: os.Getenv("XENIBASE_LOAD_EDGE_ADDR"), EdgeCA: os.Getenv("XENIBASE_LOAD_EDGE_CA")}
	if r.Email == "" || r.Password == "" || r.TOTPSecret == "" || r.APIDomain == "" {
		t.Fatal("XENIBASE_LOAD_TARGET needs XENIBASE_LOAD_EMAIL, XENIBASE_LOAD_PASSWORD, XENIBASE_LOAD_TOTP_SECRET (a platform admin) and XENIBASE_LOAD_API_DOMAIN")
	}
	return r
}

// edgeTransport reaches the target's edge: EdgeAddr in place of DNS, and
// EdgeCA's roots.
func (r *remoteTarget) edgeTransport(t *testing.T, tr *http.Transport) {
	if r.EdgeAddr != "" {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, r.EdgeAddr)
		}
	}
	if r.EdgeCA != "" {
		pem, err := os.ReadFile(r.EdgeCA)
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			t.Fatalf("%s: no certificates", r.EdgeCA)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
}

// remoteControl is a signed-in platform admin's session on the target.
type remoteControl struct {
	t      *testing.T
	r      *remoteTarget
	client *http.Client
	csrf   string
}

// signIn signs the admin in with password and TOTP.
func (r *remoteTarget) signIn(t *testing.T) *remoteControl {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &remoteControl{t: t, r: r, client: &http.Client{Jar: jar, Timeout: time.Minute}}
	var st gen.SessionState
	if code := c.Do("GET", "/api/v1/session", nil, &st); code != http.StatusOK {
		t.Fatalf("session: %d", code)
	}
	c.csrf = st.CsrfToken
	var ch gen.LoginChallenge
	if code := c.Do("POST", "/api/v1/auth/login", map[string]string{"email": r.Email, "password": r.Password}, &ch); code != http.StatusOK {
		t.Fatalf("sign in as %s: %d", r.Email, code)
	}
	code, err := totp.Code(r.TOTPSecret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Do("POST", "/api/v1/auth/totp", map[string]string{"challenge_id": *ch.ChallengeId, "code": code}, &st); s != http.StatusOK || !st.Authenticated {
		t.Fatalf("TOTP for %s: %d", r.Email, s)
	}
	if st.CsrfToken != "" {
		c.csrf = st.CsrfToken
	}
	return c
}

func (c *remoteControl) Do(method, path string, body, out any) int {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	for try := 0; ; try++ {
		if rd != nil {
			if s, ok := rd.(io.Seeker); ok {
				_, _ = s.Seek(0, io.SeekStart)
			}
		}
		req, err := http.NewRequest(method, c.r.URL+path, rd)
		if err != nil {
			c.t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if c.csrf != "" {
			req.Header.Set("X-CSRF-Token", c.csrf)
		}
		res, err := c.client.Do(req)
		if err != nil {
			c.t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		// A session is rate limited like any client: wait and retry.
		if res.StatusCode == http.StatusTooManyRequests && try < 30 {
			time.Sleep(2 * time.Second)
			continue
		}
		if out != nil && len(b) > 0 && res.StatusCode < 300 {
			if err := json.Unmarshal(b, out); err != nil {
				c.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
			}
		}
		return res.StatusCode
	}
}

// WaitOperation polls the operation until it ends (10 minutes at most).
func (c *remoteControl) WaitOperation(id uuid.UUID) gen.Operation {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for {
		var op gen.Operation
		if code := c.Do("GET", "/api/v1/operations/"+id.String(), nil, &op); code != http.StatusOK {
			c.t.Fatalf("get operation %s: %d", id, code)
		}
		if op.Status == gen.OperationStatusSucceeded || op.Status == gen.OperationStatusFailed {
			return op
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("operation %s (%s) still %s after 10 minutes", id, op.Kind, op.Status)
		}
		time.Sleep(time.Second)
	}
}

func (c *remoteControl) CreateOrg(name string) uuid.UUID {
	c.t.Helper()
	var o gen.Org
	if code := c.Do("POST", "/api/v1/orgs", map[string]string{"name": name}, &o); code != http.StatusCreated {
		c.t.Fatalf("create org %q: %d", name, code)
	}
	return o.Id
}
