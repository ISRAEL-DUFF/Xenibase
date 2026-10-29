package edge_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/edge"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/files"
	"github.com/israel-duff/xenibase/internal/jwtes"
	"github.com/israel-duff/xenibase/internal/services"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

// filesEnv is an edge serving one project's storage: its database (with
// the xb_* schemas and the project's roles) on the test Postgres server,
// its files in an in-memory S3, and a control plane that records the
// storage events the edge sends.
type filesEnv struct {
	t      *testing.T
	ed     *edge.Edge
	cfg    edgeapi.Project
	fake   *storage.Fake
	client *storage.Client
	// admin is the platform's admin in the project's database.
	admin    *pgx.Conn
	db       string
	pub, sec string
	der      []byte

	mu     sync.Mutex
	events []edgeapi.StorageEvent
}

const filesDomain = "edge.test"

func newFilesEnv(t *testing.T) *filesEnv {
	t.Helper()
	adminURL := os.Getenv(storetest.EnvVar)
	if adminURL == "" {
		t.Skipf("%s not set; skipping test that needs Postgres", storetest.EnvVar)
	}
	ctx := context.Background()
	root, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close(ctx)
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	db := "xbs_" + hex.EncodeToString(b)
	owner := db + "_owner"
	p := store.Project{DbName: db, OwnerRole: owner}
	const pw = "storage-test-password"
	roles := []string{owner, store.EdgeRole(db), store.AnonRole(db), store.UserRole(db), store.ServiceRole(db), store.AuthHookRole(db)}
	attrs := map[string]string{store.ServiceRole(db): "BYPASSRLS"}
	for _, r := range roles {
		a := attrs[r]
		if a == "" {
			a = "NOBYPASSRLS"
		}
		if _, err := root.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN NOINHERIT %s PASSWORD '%s'`, pgx.Identifier{r}.Sanitize(), a, pw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := root.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Errorf("drop the project database: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the project database: %v", err)
		}
		for _, r := range roles {
			if _, err := c.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{r}.Sanitize()); err != nil {
				t.Errorf("drop %s: %v", r, err)
			}
		}
	})
	cc, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cc.Database = db
	admin, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	for _, r := range roles {
		if _, err := admin.Exec(ctx, "GRANT CONNECT ON DATABASE "+db+" TO "+pgx.Identifier{r}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	for _, st := range services.SchemaStatements(p) {
		if _, err := admin.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}

	fake, err := storage.NewFake("files")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fake.Close)
	cl, err := storage.New(fake.Target("files"))
	if err != nil {
		t.Fatal(err)
	}
	env := &filesEnv{t: t, fake: fake, client: cl, admin: admin, db: db}
	ctl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == edgeapi.PathStorageEvent {
			var ev edgeapi.StorageEvent
			_ = json.NewDecoder(r.Body).Decode(&ev)
			env.mu.Lock()
			env.events = append(env.events, ev)
			env.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(ctl.Close)

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	env.ed = edge.New(edge.Config{Domain: filesDomain, ControlURL: ctl.URL, Secret: "edge-secret", PoolerAddr: u.Host, PoolerSSLMode: "disable",
		GarbageGrace: time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	der, jwk, err := jwtes.Generate("k1")
	if err != nil {
		t.Fatal(err)
	}
	rawJWK, _ := json.Marshal(jwk)
	env.der = der
	env.pub, env.sec = "pk_"+hex.EncodeToString(b), "sk_"+hex.EncodeToString(b)
	ref := "s" + hex.EncodeToString(b)[:7]
	logins := map[string]string{}
	for _, r := range store.RequestRoles(db) {
		logins[r] = pw
	}
	env.cfg = edgeapi.Project{Ref: ref, ProjectID: uuid.New(), OrgID: uuid.New(), Region: "eu-central", State: edgeapi.StateActive, Version: 1,
		Database: db, EdgeUser: store.EdgeRole(db), Password: pw, AnonRole: store.AnonRole(db), UserRole: store.UserRole(db),
		ServiceRole: store.ServiceRole(db), HookRole: store.AuthHookRole(db), Logins: logins,
		Keys: []edgeapi.Key{{ID: uuid.New(), Kind: edgeapi.KindPublishable, Hash: edgeapi.HashKey(env.pub)},
			{ID: uuid.New(), Kind: edgeapi.KindSecret, Hash: edgeapi.HashKey(env.sec)}},
		JWKs:     []json.RawMessage{rawJWK},
		Settings: edgeapi.Settings{StatementTimeoutMs: 5000, RatePerIP: 100000, RatePerKey: 100000},
		Storage: &edgeapi.StorageConfig{Target: fake.Target("files"), Prefix: files.Prefix(ref), SigningSecret: []byte("signing secret " + ref),
			UploadMaxBytes: 5 << 30}}
	env.ed.LoadForStorageTest(env.cfg)
	return env
}

// update changes the project's configuration as a new feed copy would.
func (f *filesEnv) update(change func(*edgeapi.Project)) {
	sc := *f.cfg.Storage
	f.cfg.Storage = &sc
	change(&f.cfg)
	f.cfg.Version++
	f.ed.LoadForStorageTest(f.cfg)
}

// user is an access token for a user of the project.
func (f *filesEnv) user(id uuid.UUID) string {
	f.t.Helper()
	tok, err := jwtes.Sign(f.der, "k1", map[string]any{"role": "user", "sub": id.String(), "aud": f.cfg.Ref,
		"exp": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

type filesReply struct {
	Code   int
	Header http.Header
	Body   []byte
	Error  struct {
		Code string `json:"code"`
	} `json:"error"`
}

func (r filesReply) String() string { return fmt.Sprintf("%d %s", r.Code, r.Body) }

// do sends a request to the project's API; key is "" (none), "pub" or
// "sec", token a user's access token. headers are name, value pairs.
func (f *filesEnv) do(method, path, key, token string, body io.Reader, headers ...string) filesReply {
	f.t.Helper()
	if body == nil {
		body = http.NoBody
	}
	req := httptest.NewRequest(method, path, body)
	req.Host = f.cfg.Ref + "." + filesDomain
	req.RemoteAddr = "192.0.2.10:1234"
	switch key {
	case "pub":
		req.Header.Set("apikey", f.pub)
	case "sec":
		req.Header.Set("apikey", f.sec)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.ed.ServeHTTP(rec, req)
	r := filesReply{Code: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}
	_ = json.Unmarshal(r.Body, &r)
	return r
}

func (f *filesEnv) json(method, path, key, token, body string) filesReply {
	f.t.Helper()
	return f.do(method, path, key, token, strings.NewReader(body), "Content-Type", "application/json")
}

// bucket makes a bucket with the secret key.
func (f *filesEnv) bucket(id string, public bool) {
	f.t.Helper()
	if r := f.json("POST", "/storage/v1/bucket", "sec", "", fmt.Sprintf(`{"id":%q,"public":%v}`, id, public)); r.Code != 201 {
		f.t.Fatalf("bucket %s: %s", id, r)
	}
}

// exec runs SQL as the platform's admin in the project's database.
func (f *filesEnv) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.admin.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// storageEvents are the storage events the edge has sent so far.
func (f *filesEnv) storageEvents() []edgeapi.StorageEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]edgeapi.StorageEvent(nil), f.events...)
}
