// Package files holds what xenibase-edge and xenibase-server share about
// backend services' storage (V4 §5): bucket and path rules, where an
// object's bytes are, the MIME checks, the signed URL tokens, and the SQL
// on xb_storage.
package files

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// MaxPath bounds an object path, in bytes.
const MaxPath = 1024

// Prefix is where a project's files are in its region's object store.
func Prefix(ref string) string { return "files/" + ref + "/" }

// ObjectKey is where an object version's bytes are, under a project's
// prefix.
func ObjectKey(prefix string, version uuid.UUID) string {
	return prefix + "objects/" + version.String()
}

// TransformsPrefix is where a version's rendered images are cached.
func TransformsPrefix(prefix string, version uuid.UUID) string {
	return prefix + "transforms/" + version.String()
}

// BucketRe is the shape of a bucket id.
var BucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// reserved are path words the edge's endpoints use after /object/.
var reserved = map[string]bool{"public": true, "sign": true, "authenticated": true, "info": true, "move": true,
	"copy": true, "list": true, "upload": true, "render": true, "bucket": true}

// ValidBucket reports whether id may name a new bucket: lowercase letters,
// digits, '_', '.', '-', not a reserved word and not shaped like an upload
// id.
func ValidBucket(id string) bool {
	if !BucketRe.MatchString(id) || reserved[id] {
		return false
	}
	_, err := uuid.Parse(id)
	return err != nil
}

// BucketRule explains ValidBucket.
const BucketRule = "a bucket id is 1–63 lowercase letters, digits, '_', '.' or '-', starting with a letter or digit, and not a reserved word"

// ValidPath reports whether p may name an object: UTF-8 segments joined by
// '/', none empty, '.' or '..', no control characters.
func ValidPath(p string) bool {
	if p == "" || len(p) > MaxPath || !utf8.ValidString(p) {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// ---- MIME ----------------------------------------------------------------------

// MIME errors.
var (
	ErrMIMEMismatch = errors.New("mime_mismatch")
	ErrMIMEInvalid  = errors.New("invalid_mime_type")
)

var mimeTypeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)

// MIMEPatternRe is an allowed-type entry: a type or "type/*".
var MIMEPatternRe = regexp.MustCompile(`^([a-z0-9][a-z0-9!#$&^_.+-]*)/([a-z0-9][a-z0-9!#$&^_.+-]*|\*)$`)

// DetectMIME decides an upload's type from what it declares and its first
// bytes: a declared image, video or audio type the bytes contradict is
// refused (MIME spoofing, V4 §13).
func DetectMIME(declared string, head []byte) (string, error) {
	declared = strings.ToLower(strings.TrimSpace(declared))
	if t, _, err := mime.ParseMediaType(declared); err == nil {
		declared = t
	} else {
		declared = ""
	}
	sniffed, _, _ := mime.ParseMediaType(http.DetectContentType(head))
	if declared == "" || declared == "application/octet-stream" {
		if len(head) == 0 {
			return "application/octet-stream", nil
		}
		return sniffed, nil
	}
	top, _, _ := strings.Cut(declared, "/")
	stop, _, _ := strings.Cut(sniffed, "/")
	if (top == "image" || top == "video" || top == "audio") && len(head) > 0 && sniffed != "application/octet-stream" && stop != top {
		// SVG sniffs as XML or text.
		if declared != "image/svg+xml" || (sniffed != "text/xml" && sniffed != "text/plain") {
			return "", fmt.Errorf("%w: the file is declared %s but its contents are %s", ErrMIMEMismatch, declared, sniffed)
		}
	}
	if !mimeTypeRe.MatchString(declared) {
		return "", fmt.Errorf("%w: the Content-Type isn't a valid MIME type", ErrMIMEInvalid)
	}
	return declared, nil
}

// MIMEAllowed checks a type against a bucket's list (empty: any).
func MIMEAllowed(allowed []string, t string) bool {
	if len(allowed) == 0 {
		return true
	}
	top, _, _ := strings.Cut(t, "/")
	for _, a := range allowed {
		if a == t || a == top+"/*" || a == "*/*" {
			return true
		}
	}
	return false
}

// ---- Signed URLs ------------------------------------------------------------------

// Token kinds.
const (
	TokenGet = "get"
	TokenPut = "put"
)

// Transform is an image transform (V4 §5.5).
type Transform struct {
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
	Resize  string `json:"resize,omitempty"`  // cover (default) | contain | fill
	Format  string `json:"format,omitempty"`  // origin (default) | webp | avif | jpeg | png
	Quality int    `json:"quality,omitempty"` // 20–100, 80 by default
}

// Token is what a signed URL carries: the project, bucket, path and expiry,
// HMAC'd with the project's storage secret, so the edge checks a download
// token without a policy query (the policies were checked when it was made).
type Token struct {
	Kind      string     `json:"k"`
	Ref       string     `json:"r"`
	Bucket    string     `json:"b"`
	Path      string     `json:"p"`
	Exp       int64      `json:"e"`
	Download  string     `json:"d,omitempty"`
	Transform *Transform `json:"t,omitempty"`
	Upsert    bool       `json:"u,omitempty"`
	Owner     *uuid.UUID `json:"o,omitempty"`
	// ID names an upload token's row in xb_storage.upload_tokens, which
	// holds the signer's role and claims and goes when the token is used.
	ID *uuid.UUID `json:"i,omitempty"`
}

// Sign makes t's token.
func Sign(secret []byte, t Token) string {
	raw, _ := json.Marshal(t)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac(secret, payload))
}

func mac(secret []byte, payload string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("xenibase storage url\x00" + payload))
	return m.Sum(nil)
}

// Verify checks tok is a live token of kind for ref's bucket/path.
func Verify(secret []byte, tok, kind, ref, bucket, path string, now time.Time) (Token, bool) {
	payload, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return Token{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(secret, payload)) {
		return Token{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Token{}, false
	}
	var t Token
	if json.Unmarshal(raw, &t) != nil || t.Kind != kind || t.Ref != ref || t.Bucket != bucket || t.Path != path || now.Unix() >= t.Exp {
		return Token{}, false
	}
	return t, true
}

// EscapePath escapes each segment of an object path for a URL.
func EscapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = escapeSegment(s)
	}
	return strings.Join(segs, "/")
}

// ---- SQL ---------------------------------------------------------------------------

// ObjectCols are an object row's columns, in Scan order.
const ObjectCols = `id, bucket, path, version, size, mime_type, etag, checksum, owner, user_metadata, created_at, updated_at`

// BucketCols are a bucket row's columns, in Scan order.
const BucketCols = `id, public, file_size_limit, allowed_mime_types, cache_seconds, created_at, updated_at`

// InsertSQL writes an object row ($1 bucket, $2 path, $3 version, $4 size,
// $5 mime_type, $6 etag, $7 checksum, $8 owner, $9 user_metadata); on
// conflict it replaces the object (upsert) or fails with a unique
// violation.
func InsertSQL(upsert bool) string {
	s := `INSERT INTO xb_storage.objects (bucket, path, version, size, mime_type, etag, checksum, owner, user_metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	if upsert {
		s += ` ON CONFLICT (bucket, path) DO UPDATE SET version = EXCLUDED.version, size = EXCLUDED.size,
		  mime_type = EXCLUDED.mime_type, etag = EXCLUDED.etag, checksum = EXCLUDED.checksum, owner = EXCLUDED.owner,
		  user_metadata = EXCLUDED.user_metadata, updated_at = now()`
	}
	return s + ` RETURNING ` + ObjectCols
}

// LikePrefix is a LIKE pattern (ESCAPE '\') matching paths under p.
func LikePrefix(p string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(p) + "%"
}

// ListFolderSQL lists a folder: the names directly under a prefix, objects
// and folders, after a cursor ($1 bucket, $2 LikePrefix, $3 the prefix's
// length in characters plus one, $4/$5 the cursor's name and folder flag,
// $6 the limit). Columns: name, folder, then an object's id, version,
// size, mime_type, etag, owner, user_metadata, created_at, updated_at (null
// for folders).
const ListFolderSQL = `SELECT name, folder,
	  CASE WHEN folder THEN NULL ELSE (array_agg(id))[1] END,
	  CASE WHEN folder THEN NULL ELSE (array_agg(version))[1] END,
	  CASE WHEN folder THEN NULL ELSE (array_agg(size))[1] END,
	  CASE WHEN folder THEN NULL ELSE (array_agg(mime_type))[1] END,
	  CASE WHEN folder THEN NULL ELSE (array_agg(etag))[1] END,
	  CASE WHEN folder THEN NULL ELSE (array_agg(owner))[1] END,
	  CASE WHEN folder THEN NULL ELSE (array_agg(user_metadata))[1] END,
	  CASE WHEN folder THEN NULL ELSE (array_agg(created_at))[1] END,
	  CASE WHEN folder THEN NULL ELSE (array_agg(updated_at))[1] END
	FROM (SELECT split_part(substr(path, $3), '/', 1) AS name, strpos(substr(path, $3), '/') > 0 AS folder, o.*
	      FROM xb_storage.objects o WHERE bucket = $1 AND path LIKE $2 ESCAPE '\') x
	WHERE (name, folder) > ($4, $5)
	GROUP BY name, folder ORDER BY name, folder LIMIT $6`

func escapeSegment(s string) string { return url.PathEscape(s) }
