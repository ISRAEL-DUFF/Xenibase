package edge_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/israel-duff/xenibase/internal/edgeapi"
)

// waitEvent waits for the edge to send a storage event like want (and
// Paths, when want has them).
func (f *filesEnv) waitEvent(t *testing.T, want edgeapi.StorageEvent) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range f.storageEvents() {
			if ev.Ref == f.cfg.Ref && ev.Event == want.Event && ev.Bucket == want.Bucket && (want.Paths == nil || slices.Equal(ev.Paths, want.Paths)) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %+v in %+v", want, f.storageEvents())
}

// The edge used to tell xenibase-server only that a bucket went private,
// so deleting, replacing or moving a public bucket's file, or deleting or
// emptying the bucket, left its URLs in the CDN's cache. Each now sends an
// event the server turns into a purge; a private bucket's changes don't.
func TestPublicFilesLeavingTheirURLsArePurged(t *testing.T) {
	f := newFilesEnv(t)
	f.bucket("site", true)
	f.bucket("private", false)
	put := func(bucket, path, body string) {
		t.Helper()
		if r := f.do("PUT", "/storage/v1/object/"+bucket+"/"+path, "sec", "", strings.NewReader(body), "Content-Type", "text/plain"); r.Code != 200 {
			t.Fatalf("upload %s/%s: %s", bucket, path, r)
		}
	}
	put("site", "a.txt", "one")
	put("private", "a.txt", "one")
	time.Sleep(10 * time.Millisecond) // a replacement's updated_at is later
	put("private", "a.txt", "two")
	if evs := f.storageEvents(); len(evs) != 0 {
		t.Fatalf("events for new files and a private bucket: %+v", evs)
	}

	put("site", "a.txt", "two")
	f.waitEvent(t, edgeapi.StorageEvent{Event: edgeapi.EventObjectsChanged, Bucket: "site", Paths: []string{"a.txt"}})

	put("site", "b.txt", "b")
	if r := f.json("POST", "/storage/v1/object/move", "sec", "", `{"bucket":"site","from":"b.txt","to":"c.txt"}`); r.Code != 200 {
		t.Fatalf("move: %s", r)
	}
	f.waitEvent(t, edgeapi.StorageEvent{Event: edgeapi.EventObjectsChanged, Bucket: "site", Paths: []string{"b.txt"}})

	if r := f.do("DELETE", "/storage/v1/object/site/c.txt", "sec", "", nil); r.Code != 200 {
		t.Fatalf("delete: %s", r)
	}
	f.waitEvent(t, edgeapi.StorageEvent{Event: edgeapi.EventObjectsChanged, Bucket: "site", Paths: []string{"c.txt"}})

	if r := f.json("POST", "/storage/v1/bucket/site/empty", "sec", "", `{}`); r.Code != 200 {
		t.Fatalf("empty: %s", r)
	}
	f.waitEvent(t, edgeapi.StorageEvent{Event: edgeapi.EventBucketEmptied, Bucket: "site"})
	if r := f.do("DELETE", "/storage/v1/bucket/site", "sec", "", nil); r.Code != 200 {
		t.Fatalf("delete the bucket: %s", r)
	}
	f.waitEvent(t, edgeapi.StorageEvent{Event: edgeapi.EventBucketDeleted, Bucket: "site"})
	for _, ev := range f.storageEvents() {
		if ev.Bucket == "private" {
			t.Fatalf("an event for the private bucket: %+v", ev)
		}
	}
}
