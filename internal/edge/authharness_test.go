package edge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/jwtes"
)

// An edge serving one project's /auth/v1 against a throwaway database in
// the Postgres that XENIBASE_TEST_DATABASE_URL names, with the xb_*
// schemas as xenibase-server applies them and a fake control plane that
// records the messages the edge asks it to send.

const authTestDomain = "auth.edge.test"

type authEdge struct {
	t    *testing.T
	e    *Edge
	ref  string
	pub  string
	sec  string
	pc   edgeapi.Project
	db   *pgx.Conn // the platform's admin, on the project's database
	mu   sync.Mutex
	sent []edgeapi.AuthMessage
	// refuse, when set, answers the edge's message requests (status, body).
	refuse func(edgeapi.AuthMessage) (int, string)
}

// schemaStatements are services/schema.go's statements, every version in
// order (read from its source: services imports this package).
func schemaStatements(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../services/schema.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "schemaVersions" {
			return true
		}
		for _, v := range vs.Values[0].(*ast.CompositeLit).Elts {
			stmts := v.(*ast.CompositeLit).Elts[1].(*ast.CompositeLit)
			for _, s := range stmts.Elts {
				lit, err := strconv.Unquote(s.(*ast.BasicLit).Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, lit)
			}
		}
		return false
	})
	if len(out) == 0 {
		t.Fatal("no schema statements found in services/schema.go")
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newAuthEdge starts an edge for a fresh project; edit changes its auth
// settings before it starts.
func newAuthEdge(t *testing.T, edit func(*edgeapi.AuthConfig)) *authEdge {
	t.Helper()
	adminURL := os.Getenv("XENIBASE_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("XENIBASE_TEST_DATABASE_URL not set; skipping test that needs Postgres")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	sfx := randHex(4)
	dbName := "xb_edgeauth_" + sfx
	roles := map[string]string{"owner": "xbt_owner_" + sfx, "anon": "xbt_anon_" + sfx, "user": "xbt_user_" + sfx,
		"service": "xbt_service_" + sfx, "edge": "xbt_edge_" + sfx, "hook": "xbt_hook_" + sfx}
	edgePW := randHex(12)
	for k, r := range roles {
		stmt := "CREATE ROLE " + r + " NOLOGIN"
		if k == "edge" {
			stmt = "CREATE ROLE " + r + " LOGIN PASSWORD '" + edgePW + "'"
		}
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName+" OWNER "+roles["owner"]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		a, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			return
		}
		defer a.Close(ctx)
		_, _ = a.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		for _, r := range roles {
			_, _ = a.Exec(ctx, "DROP ROLE IF EXISTS "+r)
		}
	})
	cfg, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = dbName
	db, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	rep := strings.NewReplacer("{{anon}}", roles["anon"], "{{user}}", roles["user"], "{{service}}", roles["service"],
		"{{edge}}", roles["edge"], "{{hook}}", roles["hook"], "{{owner}}", roles["owner"])
	for _, st := range schemaStatements(t) {
		if _, err := db.Exec(ctx, rep.Replace(st)); err != nil {
			t.Fatalf("schema: %v\n%s", err, st)
		}
	}

	x := &authEdge{t: t, ref: "auth" + sfx, pub: "xb_publishable_" + sfx, sec: "xb_secret_" + sfx, db: db}
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != edgeapi.PathAuthMessage {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var m edgeapi.AuthMessage
		_ = json.NewDecoder(r.Body).Decode(&m)
		x.mu.Lock()
		refuse := x.refuse
		x.mu.Unlock()
		if refuse != nil {
			if status, body := refuse(m); status != 0 {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
				return
			}
		}
		x.mu.Lock()
		x.sent = append(x.sent, m)
		x.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(cp.Close)

	der, jwk, err := jwtes.Generate("k" + sfx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(jwk)
	auth := edgeapi.AuthConfig{SiteURL: "https://app.example.com", SignupEnabled: true, EmailConfirm: true, MagicLinkEnabled: true,
		PasswordMinLength: 8, AccessTokenTTL: 3600, PhoneConfirm: true, PhoneCountries: []string{"NG"}, MFA: MFAOptional, ManualLinking: true}
	if edit != nil {
		edit(&auth)
	}
	x.pc = edgeapi.Project{Ref: x.ref, ProjectID: uuid.New(), OrgID: uuid.New(), State: edgeapi.StateActive, Version: 1,
		Database: dbName, EdgeUser: roles["edge"], Password: edgePW, PoolerHost: cfg.Host, PoolerPort: int(cfg.Port),
		AnonRole: roles["anon"], UserRole: roles["user"], ServiceRole: roles["service"], HookRole: roles["hook"],
		Keys: []edgeapi.Key{{ID: uuid.New(), Kind: edgeapi.KindPublishable, Hash: edgeapi.HashKey(x.pub)},
			{ID: uuid.New(), Kind: edgeapi.KindSecret, Hash: edgeapi.HashKey(x.sec)}},
		JWKs: []json.RawMessage{raw}, SigningKey: &edgeapi.SigningKey{Kid: "k" + sfx, Private: der}, Auth: auth}
	x.e = New(Config{Domain: authTestDomain, ControlURL: cp.URL, Secret: "edge-secret-0123456789abcdef", PoolerSSLMode: "prefer",
		AuthRateScale: 100})
	x.e.apply([]edgeapi.Project{x.pc})
	x.e.ready = true
	t.Cleanup(x.e.closePools)
	return x
}

// reconfigure changes the project's auth settings on the running edge.
func (x *authEdge) reconfigure(edit func(*edgeapi.AuthConfig)) {
	edit(&x.pc.Auth)
	x.pc.Version++
	x.e.apply([]edgeapi.Project{x.pc})
}

type authResp struct {
	Code   int
	Body   string
	Header http.Header
}

func (r authResp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Body), &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.Code, r.Body)
	}
	return m
}

// errCode is the error code of a refusal.
func (r authResp) errCode() string {
	var m struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.Body), &m)
	return m.Error.Code
}

// do sends a request with the publishable key (the secret key for
// /auth/v1/admin/), and token as the user's bearer token when set.
func (x *authEdge) do(method, path string, body any, token string) authResp {
	x.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(method, "http://edge"+path, rd)
	r.Host = x.ref + "." + authTestDomain
	key := x.pub
	if strings.HasPrefix(path, "/auth/v1/admin/") {
		key = x.sec
	}
	r.Header.Set("apikey", key)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	x.e.ServeHTTP(w, r)
	return authResp{Code: w.Code, Body: w.Body.String(), Header: w.Header()}
}

// last is the latest message sent to addr.
func (x *authEdge) last(addr string) (edgeapi.AuthMessage, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for i := len(x.sent) - 1; i >= 0; i-- {
		if x.sent[i].To == addr {
			return x.sent[i], true
		}
	}
	return edgeapi.AuthMessage{}, false
}

// sentTo counts the messages sent to addr.
func (x *authEdge) sentTo(addr string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for _, m := range x.sent {
		if m.To == addr {
			n++
		}
	}
	return n
}

// linkToken is the token in a message's link.
func linkToken(t *testing.T, m edgeapi.AuthMessage) string {
	t.Helper()
	u, err := url.Parse(m.Link)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("token")
}

// exec runs sql on the project's database as the platform's admin.
func (x *authEdge) exec(sql string, args ...any) {
	x.t.Helper()
	if _, err := x.db.Exec(context.Background(), sql, args...); err != nil {
		x.t.Fatal(err)
	}
}

// later moves the project's auth records back by d, as if d had passed.
func (x *authEdge) later(d time.Duration) {
	x.t.Helper()
	x.exec(`UPDATE xb_auth.one_time_codes SET created_at = created_at - $1::interval`, strconv.Itoa(int(d.Seconds()))+" seconds")
}

// signIn signs the user in with a password.
func (x *authEdge) signIn(email, password string) authResp {
	x.t.Helper()
	return x.do("POST", "/auth/v1/token?grant_type=password", map[string]any{"email": email, "password": password}, "")
}

func accessToken(t *testing.T, r authResp) string {
	t.Helper()
	if r.Code != http.StatusOK {
		t.Fatalf("not signed in: %d %s", r.Code, r.Body)
	}
	tok, _ := r.json(t)["access_token"].(string)
	if tok == "" {
		t.Fatalf("no access token: %s", r.Body)
	}
	return tok
}

// TestAuthHarness: the edge signs up, confirms and signs in a user.
func TestAuthHarness(t *testing.T) {
	x := newAuthEdge(t, nil)
	if r := x.do("POST", "/auth/v1/signup", map[string]any{"email": "ada@example.com", "password": "correct horse 1"}, ""); r.Code != 200 {
		t.Fatalf("signup: %d %s", r.Code, r.Body)
	}
	m, ok := x.last("ada@example.com")
	if !ok {
		t.Fatal("no confirmation sent")
	}
	if r := x.do("POST", "/auth/v1/verify", map[string]any{"type": "signup", "email": "ada@example.com", "token": m.Code}, ""); r.Code != 200 {
		t.Fatalf("verify: %d %s", r.Code, r.Body)
	}
	accessToken(t, x.signIn("ada@example.com", "correct horse 1"))
}
