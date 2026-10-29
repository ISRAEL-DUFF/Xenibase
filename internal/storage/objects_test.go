package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestObjectsAndPresignedMultipart(t *testing.T) {
	f, err := NewFake("files")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tg := f.Target("files")
	tg.Prefix = "xenibase"
	c, err := New(tg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := c.Put(ctx, "files/r1/objects/a", bytes.NewReader([]byte("hello world")), 11, "text/plain"); err != nil {
		t.Fatal(err)
	}
	o, err := c.Get(ctx, "files/r1/objects/a", "bytes=6-10")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(o.Body)
	_ = o.Body.Close()
	if string(got) != "world" || o.Size != 11 || o.Length != 5 || o.Range == "" {
		t.Fatalf("range read: %q %+v", got, o)
	}
	if _, err := c.Copy(ctx, "files/r1/objects/a", "files/r1/objects/b"); err != nil {
		t.Fatal(err)
	}
	if n, _, err := c.Head(ctx, "files/r1/objects/b"); err != nil || n != 11 {
		t.Fatalf("copy: %d %v", n, err)
	}
	if _, _, err := c.Head(ctx, "files/r1/objects/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing head: %v", err)
	}
	if _, err := c.Get(ctx, "files/r1/objects/nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	var keys []string
	if err := c.List(ctx, "files/r1/", func(es []Entry) error {
		for _, e := range es {
			keys = append(keys, e.Key)
		}
		return nil
	}); err != nil || len(keys) != 2 || keys[0] != "files/r1/objects/a" {
		t.Fatalf("list: %v %v", keys, err)
	}

	// A multipart upload whose parts go straight to presigned URLs.
	key := "files/r1/objects/big"
	id, err := c.StartMultipart(ctx, key, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	ups, err := c.Multiparts(ctx, "files/r1/")
	if err != nil || len(ups) != 1 || ups[0].Key != key || ups[0].ID != id {
		t.Fatalf("uploads: %+v %v", ups, err)
	}
	part1 := make([]byte, 5<<20)
	_, _ = rand.Read(part1)
	part2 := []byte("tail")
	for i, body := range [][]byte{part1, part2} {
		u, err := c.PresignPart(ctx, key, id, int32(i+1), int64(len(body)), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPut, u, bytes.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("part %d: %d", i+1, res.StatusCode)
		}
	}
	parts, err := c.Parts(ctx, key, id)
	if err != nil || len(parts) != 2 || parts[0].Size != int64(len(part1)) {
		t.Fatalf("parts: %+v %v", parts, err)
	}
	if _, err := c.CompleteMultipart(ctx, key, id, parts); err != nil {
		t.Fatal(err)
	}
	if n, _, err := c.Head(ctx, key); err != nil || n != int64(len(part1)+len(part2)) {
		t.Fatalf("completed size %d %v", n, err)
	}
	o, err = c.Get(ctx, key, "")
	if err != nil {
		t.Fatal(err)
	}
	all, _ := io.ReadAll(o.Body)
	_ = o.Body.Close()
	if !bytes.Equal(all, append(append([]byte{}, part1...), part2...)) {
		t.Fatal("multipart content differs")
	}
	if _, err := c.Parts(ctx, key, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("parts after completion: %v", err)
	}

	// Aborting.
	id2, err := c.StartMultipart(ctx, "files/r1/objects/gone", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AbortMultipart(ctx, "files/r1/objects/gone", id2); err != nil {
		t.Fatal(err)
	}
	if err := c.AbortMultipart(ctx, "files/r1/objects/gone", id2); err != nil {
		t.Fatalf("second abort: %v", err)
	}
	if ups, err := c.Multiparts(ctx, "files/r1/"); err != nil || len(ups) != 0 {
		t.Fatalf("uploads after: %+v %v", ups, err)
	}
}

// A part's URL used to sign the part number and upload id but no length,
// so it took up to the store's part limit (5 GiB) whatever the upload
// declared. Content-Length is now a signed header: a URL is good for
// exactly its part's size.
func TestPresignedPartSignsItsLength(t *testing.T) {
	c, err := New(Target{Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "files", AccessKey: "test", SecretKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sign := func(size int64) url.Values {
		t.Helper()
		u, err := c.PresignPart(ctx, "files/r1/objects/big", "upload-1", 1, size, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pu, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		return pu.Query()
	}
	a, b := sign(16<<20), sign(16<<20+1)
	if !strings.Contains(a.Get("X-Amz-SignedHeaders"), "content-length") {
		t.Fatalf("signed headers: %q", a.Get("X-Amz-SignedHeaders"))
	}
	if a.Get("X-Amz-Signature") == b.Get("X-Amz-Signature") {
		t.Fatal("parts of different sizes have the same signature")
	}
	if _, err := c.PresignPart(ctx, "files/r1/objects/big", "upload-1", 1, 0, time.Hour); err == nil {
		t.Fatal("a part without a size was signed")
	}
}
