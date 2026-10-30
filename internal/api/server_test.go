package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ui := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><title>app</title>")},
		"assets/app-1a.js": {Data: []byte("console.log(1)")},
		"favicon.svg":      {Data: []byte("<svg/>")},
	}
	h := NewHandler(Options{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		UI:             ui,
		UIIndex:        "index.html",
		InsecureNoAuth: true,
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

type response struct {
	StatusCode int
	Header     http.Header
}

func get(t *testing.T, ts *httptest.Server, path string) (response, string) {
	t.Helper()
	res, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{StatusCode: res.StatusCode, Header: res.Header}, string(b)
}

func TestVersion(t *testing.T) {
	ts := newTestServer(t)
	res, body := get(t, ts, "/api/v1/version")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	var v gen.Version
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Version == "" || v.GoVersion == "" {
		t.Fatalf("incomplete version: %+v", v)
	}
}

func TestHealth(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range []string{"/healthz", "/readyz"} {
		if res, body := get(t, ts, p); res.StatusCode != http.StatusOK || !strings.Contains(body, `"ok"`) {
			t.Errorf("%s: %d %s", p, res.StatusCode, body)
		}
	}
}

// TestNotReadyOnANewerSchema: once a newer release has migrated the
// database, /readyz says so and /healthz (liveness) doesn't.
func TestNotReadyOnANewerSchema(t *testing.T) {
	ts := httptest.NewServer(NewHandler(Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), UI: fstest.MapFS{"index.html": {Data: []byte("app")}}, UIIndex: "index.html",
		InsecureNoAuth: true, Schema: func() error { return store.ErrSchemaNewer },
	}))
	t.Cleanup(ts.Close)
	if res, body := get(t, ts, "/readyz"); res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "schema is newer") {
		t.Errorf("/readyz: %d %s", res.StatusCode, body)
	}
	if res, _ := get(t, ts, "/healthz"); res.StatusCode != http.StatusOK {
		t.Errorf("/healthz: %d", res.StatusCode)
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	ts := newTestServer(t)
	res, body := get(t, ts, "/api/v1/nope")
	if res.StatusCode != http.StatusNotFound || !strings.Contains(body, "not_found") {
		t.Fatalf("got %d %s", res.StatusCode, body)
	}
}

func TestSPA(t *testing.T) {
	ts := newTestServer(t)

	for _, p := range []string{"/", "/projects/abc", "/settings/storage"} {
		res, body := get(t, ts, p)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, "<title>app</title>") {
			t.Errorf("%s: %d %q", p, res.StatusCode, body)
		}
		if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: cache-control %q", p, cc)
		}
	}

	res, body := get(t, ts, "/assets/app-1a.js")
	if res.StatusCode != http.StatusOK || body != "console.log(1)" {
		t.Fatalf("asset: %d %q", res.StatusCode, body)
	}
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("asset cache-control %q", cc)
	}

	res, _ = get(t, ts, "/favicon.svg")
	if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("favicon cache-control %q", cc)
	}
}
