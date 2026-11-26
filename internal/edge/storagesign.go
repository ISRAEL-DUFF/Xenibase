package edge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/files"
)

// Signed URLs (V4 §5.4): a token over the project, bucket, path and expiry,
// HMAC'd with the project's storage secret, so the edge checks a download's
// without a policy query (the policies were checked when it was made). An
// upload's names a row holding its signer's role and claims: the upload is
// written as them, and spends the row.

const (
	defaultSignTTL = time.Hour
	maxSignTTL     = 7 * 24 * time.Hour
	uploadSignTTL  = 2 * time.Hour
)

// baseURL is how the caller reached the edge, for absolute URLs.
func baseURL(c *call) string {
	scheme := "https"
	if c.r.TLS == nil && c.r.Header.Get("X-Forwarded-Proto") == "http" {
		scheme = "http"
	}
	return scheme + "://" + c.r.Host
}

func objectURL(prefix, bucket, path string) string {
	return "/storage/v1/" + prefix + "/" + url.PathEscape(bucket) + "/" + files.EscapePath(path)
}

type signInput struct {
	ExpiresIn int              `json:"expires_in"`
	Download  any              `json:"download"`
	Transform *files.Transform `json:"transform"`
	Paths     []string         `json:"paths"`
	Upsert    bool             `json:"upsert"`
}

func (in signInput) ttl(def time.Duration) time.Duration {
	if in.ExpiresIn <= 0 {
		return def
	}
	return min(time.Duration(in.ExpiresIn)*time.Second, maxSignTTL)
}

func (in signInput) download() string {
	switch v := in.Download.(type) {
	case bool:
		if v {
			return "1"
		}
	case string:
		return v
	}
	return ""
}

// signDownload makes signed download URLs for objects the caller may read:
// one path in the URL, or several in the body.
func (e *Edge) signDownload(c *call, req Request, bucket, path string) {
	var in signInput
	if !authBody(c, &in) {
		return
	}
	sc, _, ok := e.store(c)
	if !ok {
		return
	}
	paths := in.Paths
	if path != "" {
		paths = []string{path}
	}
	if len(paths) == 0 || len(paths) > 1000 {
		c.fail(http.StatusBadRequest, "invalid_paths", "name 1–1000 paths to sign")
		return
	}
	for _, p := range paths {
		if !files.ValidPath(p) {
			c.fail(http.StatusBadRequest, "invalid_path", "not an object path: "+p)
			return
		}
	}
	if in.Transform != nil {
		if a := checkTransform(in.Transform); a != nil {
			a.send(c)
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), req.Timeout+3*time.Second)
	defer cancel()
	found := map[string]bool{}
	err := e.asRole(ctx, c, req, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT path FROM xb_storage.objects WHERE bucket = $1 AND path = ANY($2)`, bucket, paths)
		if err != nil {
			return err
		}
		ps, err := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, p := range ps {
			found[p] = true
		}
		return err
	})
	if err != nil {
		e.storageError(c, err)
		return
	}
	exp := time.Now().Add(in.ttl(defaultSignTTL)).Unix()
	type signed struct {
		Path      string  `json:"path"`
		SignedURL *string `json:"signed_url"`
		Error     *string `json:"error,omitempty"`
	}
	out := make([]signed, 0, len(paths))
	for _, p := range paths {
		if !found[p] {
			msg := "no such object, or the project's storage policies don't allow reading it"
			out = append(out, signed{Path: p, Error: &msg})
			continue
		}
		t := files.Token{Kind: files.TokenGet, Ref: c.p.cfg.Ref, Bucket: bucket, Path: p, Exp: exp, Download: in.download(), Transform: in.Transform}
		where := "object/sign"
		if in.Transform != nil {
			where = "render/sign"
		}
		u := baseURL(c) + objectURL(where, bucket, p) + "?token=" + files.Sign(sc.SigningSecret, t)
		out = append(out, signed{Path: p, SignedURL: &u})
	}
	if path != "" {
		if out[0].SignedURL == nil {
			c.fail(http.StatusNotFound, "object_not_found", *out[0].Error)
			return
		}
		c.json(http.StatusOK, map[string]any{"signed_url": *out[0].SignedURL, "expires_at": time.Unix(exp, 0).UTC()})
		return
	}
	c.json(http.StatusOK, out)
}

// signedDownload serves a signed download URL.
func (e *Edge) signedDownload(c *call, bucket, path string) {
	sc, cl, ok := e.store(c)
	if !ok {
		return
	}
	t, ok := files.Verify(sc.SigningSecret, c.r.URL.Query().Get("token"), files.TokenGet, c.p.cfg.Ref, bucket, path, time.Now())
	// A token with a transform is for that render only (/render/sign/):
	// a signed thumbnail must not open the original.
	if !ok || t.Transform != nil {
		c.fail(http.StatusBadRequest, "invalid_signature", "the URL's token is invalid or has expired")
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), 10*time.Second)
	defer cancel()
	o, err := e.edgeObject(ctx, c.p, bucket, path)
	if err != nil {
		e.storageError(c, err)
		return
	}
	if o == nil {
		c.fail(http.StatusNotFound, "object_not_found", "no such object")
		return
	}
	dl := t.Download
	if d := c.r.URL.Query().Get("download"); d != "" {
		dl = d
	}
	e.serveObject(c, sc, cl, o, int(time.Until(time.Unix(t.Exp, 0)).Seconds()), false, dl)
}

// signUpload makes a signed upload URL for a path the caller's policies let
// it write; whoever has the URL may upload there once, as the caller, until
// it expires (2 hours at most).
func (e *Edge) signUpload(c *call, req Request, bucket, path string) {
	var in signInput
	if !authBody(c, &in) {
		return
	}
	sc, _, ok := e.store(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), req.Timeout+3*time.Second)
	defer cancel()
	if _, err := e.bucket(ctx, c.p, bucket); err != nil {
		e.storageError(c, err)
		return
	}
	upsert := in.Upsert || c.r.Header.Get("X-Upsert") == "true"
	u := upload{bucket: bucket, path: path, mime: "application/octet-stream", owner: c.userID, upsert: upsert}
	if err := e.mayWrite(ctx, c, req, u); err != nil {
		e.storageError(c, err)
		return
	}
	pool, err := e.dbPool(ctx, c.p)
	if err != nil {
		e.dbError(c, err)
		return
	}
	// The upload is written as the signer: their role and claims wait in
	// the token's row, which using the token removes.
	id, exp := uuid.New(), time.Now().Add(min(in.ttl(uploadSignTTL), uploadSignTTL)).Unix()
	claims, err := json.Marshal(req.Claims)
	if err != nil {
		e.dbError(c, err)
		return
	}
	if _, err := pool.Exec(ctx, `DELETE FROM xb_storage.upload_tokens WHERE ctid = ANY (ARRAY(SELECT ctid FROM xb_storage.upload_tokens
		WHERE expires_at < now() LIMIT 20))`); err != nil {
		e.dbError(c, err)
		return
	}
	if _, err := pool.Exec(ctx, `INSERT INTO xb_storage.upload_tokens (id, bucket, path, role, claims, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, bucket, path, req.Role, string(claims), time.Unix(exp, 0)); err != nil {
		e.dbError(c, err)
		return
	}
	tok := files.Sign(sc.SigningSecret, files.Token{Kind: files.TokenPut, Ref: c.p.cfg.Ref, Bucket: bucket, Path: path, Exp: exp,
		Upsert: upsert, Owner: c.userID, ID: &id})
	c.json(http.StatusOK, map[string]any{"url": baseURL(c) + objectURL("upload/sign", bucket, path) + "?token=" + tok,
		"token": tok, "path": path, "expires_at": time.Unix(exp, 0).UTC()})
}

// errTokenSpent is a signed upload URL that has been used, or has expired
// since it was checked.
var errTokenSpent = refuse(http.StatusBadRequest, "invalid_signature", "the URL's token has been used already or has expired")

// signedUpload takes an upload to a signed upload URL, as its signer.
func (e *Edge) signedUpload(c *call, bucket, path string) {
	sc, _, ok := e.store(c)
	if !ok {
		return
	}
	tok := c.r.URL.Query().Get("token")
	if tok == "" {
		tok = strings.TrimPrefix(c.r.Header.Get("Authorization"), "Bearer ")
	}
	t, ok := files.Verify(sc.SigningSecret, tok, files.TokenPut, c.p.cfg.Ref, bucket, path, time.Now())
	// A token without a row (made by an older edge) can't be spent.
	if !ok || t.ID == nil {
		c.fail(http.StatusBadRequest, "invalid_signature", "the URL's token is invalid or has expired")
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), 10*time.Second)
	defer cancel()
	pool, err := e.dbPool(ctx, c.p)
	if err != nil {
		e.dbError(c, err)
		return
	}
	req := Request{Timeout: time.Duration(c.p.cfg.Settings.StatementTimeoutMs) * time.Millisecond}
	var claims []byte
	err = pool.QueryRow(ctx, `SELECT role, claims FROM xb_storage.upload_tokens WHERE id = $1 AND bucket = $2 AND path = $3 AND expires_at > now()`,
		*t.ID, bucket, path).Scan(&req.Role, &claims)
	if errors.Is(err, pgx.ErrNoRows) {
		errTokenSpent.send(c)
		return
	}
	if err == nil {
		err = json.Unmarshal(claims, &req.Claims)
	}
	if err != nil {
		e.dbError(c, err)
		return
	}
	e.putObject(c, req, bucket, path, t.Upsert, t.Owner, t.ID)
}
