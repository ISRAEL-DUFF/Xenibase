package edge_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/israel-duff/xenibase/internal/edge"
	"github.com/israel-duff/xenibase/internal/edgeapi"
)

type started struct {
	ID    string `json:"id"`
	Parts []struct {
		Number int32  `json:"number"`
		Size   int64  `json:"size"`
		URL    string `json:"url"`
	} `json:"parts"`
}

func (f *filesEnv) startUpload(t *testing.T, path string, size int64) (started, filesReply) {
	t.Helper()
	r := f.json("POST", "/storage/v1/upload/big/"+path, "sec", "", fmt.Sprintf(`{"size":%d,"mime_type":"application/octet-stream"}`, size))
	var s started
	if r.Code == 200 {
		if err := json.Unmarshal(r.Body, &s); err != nil {
			t.Fatal(err)
		}
	}
	return s, r
}

// Multipart uploads used to be neither bounded nor counted: a part's URL
// signed no length, so each took up to the store's part limit whatever
// the upload declared, and open uploads weren't in the quota check, so
// uploads started together each passed against the same total. Now each
// part's URL is good for exactly its size, open uploads hold their
// declared sizes against the quota (direct uploads see them too), and a
// project has a bounded number open.
func TestMultipartUploadsAreBoundedAndCounted(t *testing.T) {
	f := newFilesEnv(t)
	f.bucket("big", false)
	const mib = 1 << 20
	f.update(func(p *edgeapi.Project) { p.Storage.QuotaBytes = 100 * mib })

	a, r := f.startUpload(t, "a.bin", 60*mib)
	if r.Code != 200 {
		t.Fatalf("start a: %s", r)
	}
	var total int64
	for _, p := range a.Parts {
		u, err := url.Parse(p.URL)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), "content-length") {
			t.Fatalf("part %d's URL doesn't sign its length: %s", p.Number, p.URL)
		}
		total += p.Size
	}
	if len(a.Parts) != 4 || total != 60*mib || a.Parts[3].Size != 12*mib {
		t.Fatalf("parts: %+v", a.Parts)
	}

	// The open upload holds 60 MiB of the 100.
	if _, r := f.startUpload(t, "b.bin", 60*mib); r.Code != 413 || r.Error.Code != "quota_exceeded" {
		t.Fatalf("a second upload past the quota: %s", r)
	}
	if r := f.do("POST", "/storage/v1/object/big/c.bin", "sec", "", bytes.NewReader(make([]byte, 45*mib)), "Content-Type", "application/octet-stream"); r.Code != 413 || r.Error.Code != "quota_exceeded" {
		t.Fatalf("a direct upload past the quota: %s", r)
	}
	if r := f.do("DELETE", "/storage/v1/upload/"+a.ID, "sec", "", nil); r.Code != 200 {
		t.Fatalf("abort a: %s", r)
	}
	if _, r := f.startUpload(t, "b.bin", 60*mib); r.Code != 200 {
		t.Fatalf("start b once a is aborted: %s", r)
	}

	// Open uploads are capped per project.
	defer edge.SetMaxOpenUploads(2)()
	if _, r := f.startUpload(t, "c.bin", 1024); r.Code != 200 {
		t.Fatalf("start c: %s", r)
	}
	if _, r := f.startUpload(t, "d.bin", 1024); r.Code != 429 || r.Error.Code != "too_many_uploads" {
		t.Fatalf("a third open upload: %s", r)
	}
}
