package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"strings"
	"testing"
)

func TestUploadDownloadDelete(t *testing.T) {
	f, err := NewFake("backups")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tg := f.Target("backups")
	tg.Prefix = "xenibase"
	c, err := New(tg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	steps, ok := c.LiveTest(ctx)
	if !ok || len(steps) != 4 {
		t.Fatalf("live test: %+v", steps)
	}

	// Larger than one multipart part, streamed from a reader of unknown size.
	data := make([]byte, 17<<20)
	_, _ = rand.Read(data)
	if err := c.Upload(ctx, "projects/x/logical/1.dump.enc", io.MultiReader(bytes.NewReader(data))); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Size(ctx, "projects/x/logical/1.dump.enc"); err != nil || n != int64(len(data)) {
		t.Fatalf("size %d %v", n, err)
	}
	rc, err := c.Download(ctx, "projects/x/logical/1.dump.enc")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("round trip mismatch")
	}
	// The prefix is applied.
	if _, err := f.backend.HeadObject("backups", "xenibase/projects/x/logical/1.dump.enc"); err != nil {
		t.Fatalf("prefix not applied: %v", err)
	}
	if err := c.Delete(ctx, "projects/x/logical/1.dump.enc"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Size(ctx, "projects/x/logical/1.dump.enc"); err == nil {
		t.Fatal("object still exists")
	}
	if err := c.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
}

func TestLiveTestFailsWithBadCredentialsOrBucket(t *testing.T) {
	f, _ := NewFake("backups")
	defer f.Close()
	tg := f.Target("missing-bucket")
	c, _ := New(tg)
	steps, ok := c.LiveTest(context.Background())
	if ok || len(steps) != 1 || steps[0].OK {
		t.Fatalf("expected the write to fail: %+v", steps)
	}
}

func TestValidate(t *testing.T) {
	good := Target{Endpoint: "https://x.r2.cloudflarestorage.com", Bucket: "b", AccessKey: "a", SecretKey: "s"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Target{
		{Endpoint: "ftp://x", Bucket: "b", AccessKey: "a", SecretKey: "s"},
		{Endpoint: "https://x", Bucket: "", AccessKey: "a", SecretKey: "s"},
		{Endpoint: "https://x", Bucket: "a/b", AccessKey: "a", SecretKey: "s"},
		{Endpoint: "https://x", Bucket: "b"},
		{Endpoint: "https://x", Bucket: "b", AccessKey: "a", SecretKey: "s", Prefix: "/p"},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if r := good.Redacted(); r.SecretKey != "" {
		t.Fatal("secret not redacted")
	}
	if k := (Target{Prefix: "p"}).Key("/a/b"); k != "p/a/b" {
		t.Fatalf("key %q", k)
	}
}

func TestDeletePrefix(t *testing.T) {
	f, err := NewFake("b")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tgt := f.Target("b")
	tgt.Prefix = "pg"
	c, err := New(tgt)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, k := range []string{"instances/a/wal-g/1", "instances/a/wal-g/x/2", "instances/ab/keep"} {
		if err := c.Upload(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	n, err := c.DeletePrefix(ctx, "instances/a")
	if err != nil || n != 2 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if _, err := c.Size(ctx, "instances/ab/keep"); err != nil {
		t.Fatal("deleted an object outside the prefix")
	}
}

// Targets that differ only in how their endpoint or prefix is written have
// one location; another bucket, prefix or host is another location.
func TestLocation(t *testing.T) {
	base := Target{Endpoint: "https://s3.example.com", Bucket: "b", Prefix: "backups"}
	same := []Target{
		{Endpoint: "https://s3.example.com/", Bucket: "b", Prefix: "backups/"},
		{Endpoint: "HTTPS://S3.Example.com:443", Bucket: "b", Prefix: "backups", AccessKey: "other"},
	}
	for _, o := range same {
		if o.Location() != base.Location() {
			t.Errorf("%+v: location %q, want %q", o, o.Location(), base.Location())
		}
	}
	other := []Target{
		{Endpoint: "https://s3.example.com", Bucket: "c", Prefix: "backups"},
		{Endpoint: "https://s3.example.com", Bucket: "b", Prefix: "backups/x"},
		{Endpoint: "https://s3.example.com", Bucket: "b"},
		{Endpoint: "https://s3.example.com:9000", Bucket: "b", Prefix: "backups"},
	}
	for _, o := range other {
		if o.Location() == base.Location() {
			t.Errorf("%+v: same location as %+v", o, base)
		}
	}
}
