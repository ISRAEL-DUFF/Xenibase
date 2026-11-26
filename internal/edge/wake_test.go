package edge

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/edgeapi"
)

// TestPausedProjectWakesOnlyWithAKey: a paused project's hostname is
// public, so a request without one of its live keys answers 503 and asks
// the control plane for nothing; one with a key wakes it.
func TestPausedProjectWakesOnlyWithAKey(t *testing.T) {
	var wakes atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == edgeapi.PathWake {
			wakes.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	const ref, domain, key = "sleepy01", "edge.test", "xb_publishable_sleepy"
	e := New(Config{Domain: domain, ControlURL: cp.URL, Secret: "edge-secret-0123456789abcdef"})
	e.apply([]edgeapi.Project{{Ref: ref, ProjectID: uuid.New(), OrgID: uuid.New(), State: edgeapi.StatePaused, Version: 1,
		Keys: []edgeapi.Key{{ID: uuid.New(), Kind: edgeapi.KindPublishable, Hash: edgeapi.HashKey(key)}}}})
	e.ready = true

	get := func(path, apikey string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://edge"+path, nil)
		r.Host = ref + "." + domain
		if apikey != "" {
			r.Header.Set("apikey", apikey)
		}
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w
	}
	waited := func() int32 {
		time.Sleep(200 * time.Millisecond) // the wake is sent in the background
		return wakes.Load()
	}
	for _, c := range []struct{ path, key string }{
		{"/data/v1/notes", ""},
		{"/data/v1/notes", "not-a-key"},
		{"/storage/v1/object/public/avatars/a.png", ""},
	} {
		if w := get(c.path, c.key); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s key=%q: %d %s", c.path, c.key, w.Code, w.Body)
		}
	}
	if n := waited(); n != 0 {
		t.Fatalf("requests without a live key woke the project %d times", n)
	}
	if w := get("/data/v1/notes", key); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("with the key: %d %s", w.Code, w.Body)
	}
	if n := waited(); n != 1 {
		t.Fatalf("a request with the key: %d wakes, want 1", n)
	}
}
