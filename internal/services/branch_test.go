package services

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/storage"
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
