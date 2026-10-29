package edge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/store"
)

// signUpload asks for a signed upload URL as token's user and returns its
// path and query.
func (f *filesEnv) signUpload(t *testing.T, token, path, body string) (string, time.Time) {
	t.Helper()
	r := f.json("POST", "/storage/v1/object/upload/sign/docs/"+path, "pub", token, body)
	var out struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if r.Code != 200 || json.Unmarshal(r.Body, &out) != nil {
		t.Fatalf("sign an upload of %s: %s", path, r)
	}
	u, err := url.Parse(out.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI(), out.ExpiresAt
}

// A signed upload URL used to be committed with the edge's own login, which
// passes every policy, so a policy on what is written (type, size,
// metadata) never applied; and nothing spent the token, so an upsert URL
// overwrote its path as often as its holder liked, for up to 7 days. The
// upload is now written as the signer's role and claims, saved with the
// token, the URL works once, and it lasts 2 hours at most.
func TestSignedUploadIsTheSignersAndSingleUse(t *testing.T) {
	f := newFilesEnv(t)
	f.bucket("docs", false)
	alice, bob := uuid.New(), uuid.New()
	// Users write in their own folder, anything but HTML.
	f.exec(fmt.Sprintf(`CREATE POLICY own_docs ON xb_storage.objects FOR ALL TO %q
		USING (bucket = 'docs' AND xb_storage.folder(path, 1) = xb_auth.uid()::text)
		WITH CHECK (bucket = 'docs' AND xb_storage.folder(path, 1) = xb_auth.uid()::text AND mime_type <> 'text/html')`, store.UserRole(f.db)))
	as := f.user(alice)
	path := alice.String() + "/notes.txt"

	// Bob may not sign for alice's folder.
	if r := f.json("POST", "/storage/v1/object/upload/sign/docs/"+path, "pub", f.user(bob), `{}`); r.Code != 403 {
		t.Fatalf("bob signs for alice's folder: %s", r)
	}

	// Seven days are asked for; two hours are given.
	put, exp := f.signUpload(t, as, path, `{"upsert":true,"expires_in":604800}`)
	if time.Until(exp) > 2*time.Hour+time.Minute {
		t.Fatalf("the upload URL lasts until %s", exp)
	}
	if r := f.do("PUT", put, "", "", strings.NewReader("first"), "Content-Type", "text/plain"); r.Code != 200 {
		t.Fatalf("upload: %s", r)
	}
	var owner *uuid.UUID
	if err := f.admin.QueryRow(context.Background(), `SELECT owner FROM xb_storage.objects WHERE path = $1`, path).Scan(&owner); err != nil ||
		owner == nil || *owner != alice {
		t.Fatalf("the object's owner: %v %v", owner, err)
	}
	// Used once: an upsert URL doesn't overwrite again.
	if r := f.do("PUT", put, "", "", strings.NewReader("second"), "Content-Type", "text/plain"); r.Code != 400 || r.Error.Code != "invalid_signature" {
		t.Fatalf("the URL used again: %s", r)
	}
	if r := f.do("GET", "/storage/v1/object/docs/"+path, "pub", as, nil); r.Code != 200 || string(r.Body) != "first" {
		t.Fatalf("the file: %s", r)
	}

	// The signer's policy on what is written applies to the upload.
	put, _ = f.signUpload(t, as, alice.String()+"/page.html", `{}`)
	if r := f.do("PUT", put, "", "", strings.NewReader("<html></html>"), "Content-Type", "text/html"); r.Code != 403 || r.Error.Code != "not_allowed" {
		t.Fatalf("an upload the policy refuses: %s", r)
	}
	if objs, err := f.fake.Objects("files"); err != nil || len(objs) != 1 {
		t.Fatalf("stored: %v %v", objs, err)
	}
	// A refused upload doesn't spend the URL.
	if r := f.do("PUT", put, "", "", strings.NewReader("hello"), "Content-Type", "text/plain"); r.Code != 200 {
		t.Fatalf("the URL after a refused upload: %s", r)
	}
}
