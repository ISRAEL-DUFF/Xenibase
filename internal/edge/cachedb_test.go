package edge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/datacat"
	"github.com/israel-duff/xenibase/internal/edgeapi"
)

// TestCacheDropsWithAPolicy: dropping the policy that let anon read a
// cached table empties the cache at the next catalog check, which cache
// hits wait for too: the rows stop being served within the check's
// interval, not the TTL. Needs a Postgres superuser in
// XENIBASE_TEST_DATABASE_URL.
func TestCacheDropsWithAPolicy(t *testing.T) {
	adminURL := os.Getenv("XENIBASE_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("XENIBASE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	root, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	db := "p_cache" + hex.EncodeToString(b)
	anon, service := db+"_anon", db+"_service"
	id := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	t.Cleanup(func() {
		_, _ = root.Exec(ctx, "DROP DATABASE IF EXISTS "+id(db)+" WITH (FORCE)")
		for _, r := range []string{anon, service} {
			_, _ = root.Exec(ctx, "DROP ROLE IF EXISTS "+id(r))
		}
		_ = root.Close(ctx)
	})
	for _, q := range []string{
		"CREATE ROLE " + id(anon) + " LOGIN NOINHERIT PASSWORD 'pw'",
		"CREATE ROLE " + id(service) + " LOGIN NOINHERIT BYPASSRLS PASSWORD 'pw'",
		"CREATE DATABASE " + id(db),
	} {
		if _, err := root.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgx.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	exec := func(stmts ...string) {
		t.Helper()
		for _, q := range stmts {
			if _, err := admin.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	exec(`CREATE TABLE items (id int PRIMARY KEY, name text)`, `INSERT INTO items VALUES (1, 'secret plan')`,
		`ALTER TABLE items ENABLE ROW LEVEL SECURITY`, `CREATE POLICY anyone ON items FOR SELECT USING (true)`,
		"GRANT USAGE ON SCHEMA public TO "+id(anon)+", "+id(service),
		"GRANT SELECT ON items TO "+id(anon)+", "+id(service))

	fp := func() string {
		t.Helper()
		var s string
		if err := admin.QueryRow(ctx, datacat.FingerprintSQL, []string{"public"}).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	e := New(Config{Domain: "edge.test", PoolerAddr: net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))), PoolerSSLMode: "disable",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	const key = "xb_pub_cachetestcachetestcachetest01"
	ref := "c" + hex.EncodeToString(b)[:7]
	e.apply([]edgeapi.Project{{Ref: ref, State: edgeapi.StateActive, Version: 1, Database: db, AnonRole: anon, ServiceRole: service,
		Logins: map[string]string{anon: "pw", service: "pw"}, Keys: []edgeapi.Key{{ID: uuid.New(), Kind: edgeapi.KindPublishable, Hash: edgeapi.HashKey(key)}},
		Settings: edgeapi.Settings{StatementTimeoutMs: 8000, RatePerIP: 1000, RatePerKey: 1000, CacheTTLSeconds: map[string]int{"public.items": 3600}}}})
	e.ready = true
	defer e.apply([]edgeapi.Project{{Ref: ref, State: edgeapi.StateDisabled, Version: 2}})
	get := func() (string, string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "http://"+ref+".edge.test/data/v1/items", nil)
		r.Header.Set("apikey", key)
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET items: %d %s", w.Code, w.Body)
		}
		return w.Header().Get("X-Cache"), w.Body.String()
	}
	if c, body := get(); c != "MISS" || !strings.Contains(body, "secret plan") {
		t.Fatalf("first read: %s %s", c, body)
	}
	if c, _ := get(); c != "HIT" {
		t.Fatalf("second read: %s", c)
	}

	// The owner closes the policy: the fingerprint sees it.
	before := fp()
	exec(`DROP POLICY anyone ON items`)
	if fp() == before {
		t.Fatal("the fingerprint missed a dropped policy")
	}
	// Within the check's interval a hit is still served; once it is due,
	// the read goes to the database, which drops the stale entry.
	e.lookup(ref).catalog.mu.Lock()
	e.lookup(ref).catalog.checked = time.Now().Add(-time.Minute)
	e.lookup(ref).catalog.mu.Unlock()
	if c, body := get(); c != "MISS" || strings.Contains(body, "secret plan") {
		t.Fatalf("after the policy went: %s %s", c, body)
	}
	e.cache.mu.Lock()
	n := len(e.cache.byKey)
	e.cache.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d entries left after the drop", n) // the read that saw the change doesn't store either
	}
	get()
	if c, body := get(); c != "HIT" || strings.Contains(body, "secret plan") {
		t.Fatalf("cached again: %s %s", c, body)
	}

	// A grant taken away, or row-level security turned off, changes it too.
	for _, q := range []string{"REVOKE SELECT ON items FROM " + id(anon), `ALTER TABLE items DISABLE ROW LEVEL SECURITY`,
		`CREATE POLICY again ON items FOR SELECT USING (false)`} {
		before := fp()
		exec(q)
		if fp() == before {
			t.Errorf("the fingerprint missed %s", q)
		}
	}
}
