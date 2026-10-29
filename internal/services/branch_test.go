package services

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store/storetest"
)

func fakeFiles(t *testing.T) (*storage.Fake, *storage.Client) {
	t.Helper()
	f, err := storage.NewFake("files")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	cl, err := storage.New(f.Target("files"))
	if err != nil {
		t.Fatal(err)
	}
	return f, cl
}

// TestCopyBranchFilesRetriesFailures: only a file the parent no longer has
// counts as missing. Any other error fails the attempt, after the other
// files were tried, and the next attempt copies only what is left.
func TestCopyBranchFilesRetriesFailures(t *testing.T) {
	ctx := context.Background()
	f, cl := fakeFiles(t)
	a, b, gone := uuid.New(), uuid.New(), uuid.New()
	for _, v := range []uuid.UUID{a, b} {
		if _, err := cl.Put(ctx, objectsKey("parent", v), bytes.NewReader([]byte("data")), 4, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	versions := []uuid.UUID{a, gone, b}

	// The store unreachable: nothing is copied, and nothing is missing.
	down, err := storage.New(storage.Target{Endpoint: "http://127.0.0.1:1", Region: "us-east-1", Bucket: "files", AccessKey: "test", SecretKey: "test", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	n, err := copyObjects(ctx, down, "parent", "branch", versions)
	if err == nil || n.missing != 0 || n.failed != 3 {
		t.Fatalf("with the store down: %+v %v", n, err)
	}

	// The operation's deadline: the attempt stops, as a failure.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if n, err := copyObjects(cancelled, cl, "parent", "branch", versions); !errors.Is(err, context.Canceled) || n.missing != 0 {
		t.Fatalf("with the context ended: %+v %v", n, err)
	}

	n, err = copyObjects(ctx, cl, "parent", "branch", versions)
	if err != nil || n.copied != 2 || n.missing != 1 {
		t.Fatalf("copy: %+v %v", n, err)
	}
	// A retry skips what is there.
	if n, err = copyObjects(ctx, cl, "parent", "branch", versions); err != nil || n.copied != 0 || n.present != 2 || n.missing != 1 {
		t.Fatalf("again: %+v %v", n, err)
	}
	keys, err := f.Objects("files")
	if err != nil || len(keys) != 4 {
		t.Fatalf("objects: %v %v", keys, err)
	}
}

// TestResetBranchFilesAreRemoved: after a reset, the files stored under the
// branch's prefix that no refilled row names (its own uploads, copies the
// parent has since replaced) go to the garbage list and the sweep removes
// them; those still named, uploading, or stored since stay.
func TestResetBranchFilesAreRemoved(t *testing.T) {
	ctx := context.Background()
	pool := storetest.NewEmpty(t)
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	conn := c.Conn()
	for _, sql := range []string{
		`CREATE SCHEMA xb_storage`,
		`CREATE TABLE xb_storage.objects (version uuid NOT NULL UNIQUE)`,
		`CREATE TABLE xb_storage.uploads (version uuid NOT NULL UNIQUE)`,
		`CREATE TABLE xb_storage.garbage (version uuid PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	f, cl := fakeFiles(t)
	put := func(v uuid.UUID) {
		t.Helper()
		if _, err := cl.Put(ctx, objectsKey("branch", v), bytes.NewReader([]byte("data")), 4, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	named, own, uploading, since := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, v := range []uuid.UUID{named, own, uploading} {
		put(v)
	}
	time.Sleep(1100 * time.Millisecond) // the store's times are to the second
	resetAt := time.Now()
	time.Sleep(1100 * time.Millisecond)
	put(since)
	// The refilled rows: the parent's still names one of the copies.
	if _, err := conn.Exec(ctx, `INSERT INTO xb_storage.objects VALUES ($1)`, named); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO xb_storage.uploads VALUES ($1)`, uploading); err != nil {
		t.Fatal(err)
	}

	n, err := strandedFiles(ctx, conn, cl, "branch", resetAt)
	if err != nil || n != 1 {
		t.Fatalf("stranded files: %d %v", n, err)
	}
	if _, err := collectGarbage(ctx, conn, cl, "branch", 0); err != nil {
		t.Fatal(err)
	}
	keys, err := f.Objects("files")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{objectsKey("branch", named): true, objectsKey("branch", uploading): true, objectsKey("branch", since): true}
	if len(keys) != len(want) {
		t.Fatalf("after the sweep: %v", keys)
	}
	for _, k := range keys {
		if !want[k] {
			t.Fatalf("after the sweep: %v (the branch's own upload should be gone)", keys)
		}
	}
}
