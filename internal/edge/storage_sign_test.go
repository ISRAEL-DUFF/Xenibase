package edge_test

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"strings"
	"testing"
)

// pngOf is a w×h PNG.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// signedPath is a signed URL's path and query, as the edge is asked for it.
func signedPath(t *testing.T, r filesReply) string {
	t.Helper()
	var out struct {
		SignedURL string `json:"signed_url"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body, &out) != nil || out.SignedURL == "" {
		t.Fatalf("sign: %s", r)
	}
	u, err := url.Parse(out.SignedURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

// A signed URL for a thumbnail used to open the original: the token was a
// plain download token with a transform inside, and /object/sign/ served
// it as one. A token with a transform is now refused there, and one
// without a transform on /render/sign/.
func TestSignedThumbnailDoesNotOpenTheOriginal(t *testing.T) {
	f := newFilesEnv(t)
	f.bucket("pics", false)
	img := pngOf(t, 400, 300)
	if r := f.do("POST", "/storage/v1/object/pics/a.png", "sec", "", bytes.NewReader(img), "Content-Type", "image/png"); r.Code != 200 {
		t.Fatalf("upload: %s", r)
	}
	thumb := signedPath(t, f.json("POST", "/storage/v1/object/sign/pics/a.png", "sec", "", `{"transform":{"width":50,"height":50}}`))
	if !strings.HasPrefix(thumb, "/storage/v1/render/sign/pics/a.png?") {
		t.Fatalf("the thumbnail's URL: %s", thumb)
	}
	if r := f.do("GET", thumb, "", "", nil); r.Code != 200 || bytes.Equal(r.Body, img) {
		t.Fatalf("the thumbnail: %d (%d bytes)", r.Code, len(r.Body))
	}
	orig := strings.Replace(thumb, "/render/sign/", "/object/sign/", 1)
	if r := f.do("GET", orig, "", "", nil); r.Code != 400 || r.Error.Code != "invalid_signature" {
		t.Fatalf("the thumbnail's token on the download route: %d (%d bytes)", r.Code, len(r.Body))
	}

	// A download token stays a download token.
	full := signedPath(t, f.json("POST", "/storage/v1/object/sign/pics/a.png", "sec", "", `{}`))
	if r := f.do("GET", full, "", "", nil); r.Code != 200 || !bytes.Equal(r.Body, img) {
		t.Fatalf("the download: %d (%d bytes)", r.Code, len(r.Body))
	}
	if r := f.do("GET", strings.Replace(full, "/object/sign/", "/render/sign/", 1)+"&width=50", "", "", nil); r.Code != 400 || r.Error.Code != "invalid_signature" {
		t.Fatalf("a download token on the render route: %s", r)
	}
}
