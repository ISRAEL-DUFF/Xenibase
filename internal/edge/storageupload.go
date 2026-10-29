package edge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/storage"
)

// Large uploads (V4 §5.3): the edge checks the policies and quota, starts a
// multipart upload in the object store and hands out presigned URLs for
// its parts, so the bytes go straight to the store; completing writes the
// row as the caller who started it. Parts can be retried one at a time, and
// an upload not completed in 24 hours is abandoned.

const (
	uploadTTL   = 24 * time.Hour
	minPartSize = 16 << 20
	maxParts    = 10000
)

// maxOpenUploads caps a project's large uploads in progress: each holds a
// share of the quota (its declared size) until it completes, is aborted or
// expires.
var maxOpenUploads = 100

type startInput struct {
	Size     int64           `json:"size"`
	MIMEType string          `json:"mime_type"`
	Upsert   bool            `json:"upsert"`
	Metadata json.RawMessage `json:"metadata"`
}

// session is a large upload in progress.
type session struct {
	ID        uuid.UUID
	Bucket    string
	Path      string
	Version   uuid.UUID
	UploadID  string
	Size      int64
	PartSize  int64
	MIMEType  string
	Upsert    bool
	Owner     *uuid.UUID
	Role      string
	Claims    map[string]any
	Metadata  json.RawMessage
	ExpiresAt time.Time
}

func (s *session) parts() int32 {
	return int32((s.Size + s.PartSize - 1) / s.PartSize)
}

// multipart serves /storage/v1/upload/... after the key check.
func (e *Edge) multipart(c *call, req Request, tail string) {
	if id, sub, _ := strings.Cut(tail, "/"); isUUID(id) && !strings.Contains(sub, "/") {
		uid, _ := uuid.Parse(id)
		switch {
		case sub == "" && c.r.Method == http.MethodGet:
			e.uploadStatus(c, uid)
		case sub == "" && c.r.Method == http.MethodDelete:
			e.abortUpload(c, uid)
		case sub == "complete" && c.r.Method == http.MethodPost:
			e.completeUpload(c, uid)
		default:
			c.fail(http.StatusNotFound, "no_such_endpoint", "no such endpoint")
		}
		return
	}
	if c.r.Method != http.MethodPost {
		c.fail(http.StatusMethodNotAllowed, "method_not_allowed", "start an upload with POST")
		return
	}
	bucket, path, ok := splitBucketPath(tail)
	if !ok {
		c.fail(http.StatusBadRequest, "invalid_path", "the URL must name a bucket and an object path")
		return
	}
	e.startUpload(c, req, bucket, path)
}

func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil && len(s) == 36
}

func (e *Edge) startUpload(c *call, req Request, bucket, path string) {
	var in startInput
	if !authBody(c, &in) {
		return
	}
	sc, cl, ok := e.store(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), 30*time.Second)
	defer cancel()
	b, err := e.bucket(ctx, c.p, bucket)
	if err != nil {
		e.storageError(c, err)
		return
	}
	if in.Size <= 0 {
		c.fail(http.StatusBadRequest, "invalid_size", "size (bytes) is required")
		return
	}
	if err := e.checkSize(ctx, c, sc, b, in.Size, sc.UploadMaxBytes); err != nil {
		e.storageError(c, err)
		return
	}
	mt, err := mimeFor(in.MIMEType, nil)
	if err != nil {
		e.storageError(c, err)
		return
	}
	if !mimeAllowed(b, mt) {
		c.fail(http.StatusUnsupportedMediaType, "mime_not_allowed", fmt.Sprintf("this bucket doesn't take %s files", mt))
		return
	}
	md := in.Metadata
	if len(md) == 0 {
		md = json.RawMessage(`{}`)
	}
	if len(md) > 8<<10 || !jsonObject(md) {
		c.fail(http.StatusBadRequest, "invalid_metadata", "metadata must be a JSON object of at most 8 KB")
		return
	}
	u := upload{bucket: bucket, path: path, size: in.Size, mime: mt, owner: c.userID, metadata: md, upsert: in.Upsert}
	if err := e.mayWrite(ctx, c, req, u); err != nil {
		e.storageError(c, err)
		return
	}
	s := &session{ID: uuid.New(), Bucket: bucket, Path: path, Version: uuid.New(), Size: in.Size, MIMEType: mt, Upsert: in.Upsert,
		Owner: c.userID, Role: req.Role, Claims: req.Claims, Metadata: md, ExpiresAt: time.Now().Add(uploadTTL)}
	s.PartSize = max(int64(minPartSize), (in.Size+maxParts-1)/maxParts)
	s.UploadID, err = cl.StartMultipart(ctx, objectKey(sc, s.Version), mt)
	if err != nil {
		e.storageError(c, storeErr(err))
		return
	}
	pool, err := e.dbPool(ctx, c.p)
	if err != nil {
		e.dbError(c, err)
		return
	}
	claims, _ := json.Marshal(s.Claims)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Starts take turns on the usage row, so uploads started together
		// can't each pass against the same total: each counts the others.
		var used, open int64
		if err := tx.QueryRow(ctx, `SELECT bytes FROM xb_storage.usage FOR UPDATE`).Scan(&used); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var inFlight int64
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(sum(size), 0)::bigint FROM xb_storage.uploads WHERE expires_at > now()`).Scan(&open, &inFlight); err != nil {
			return err
		}
		if open >= int64(maxOpenUploads) {
			return refuse(http.StatusTooManyRequests, "too_many_uploads", fmt.Sprintf("the project has %d uploads in progress: complete or abort some first", open))
		}
		if sc.QuotaBytes > 0 && used+inFlight+s.Size > sc.QuotaBytes {
			return errQuota
		}
		_, err := tx.Exec(ctx, `INSERT INTO xb_storage.uploads (id, bucket, path, version, upload_id, size, part_size, mime_type,
			upsert, owner, role, claims, user_metadata, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			s.ID, s.Bucket, s.Path, s.Version, s.UploadID, s.Size, s.PartSize, s.MIMEType, s.Upsert, s.Owner, s.Role, string(claims), s.Metadata, s.ExpiresAt)
		return err
	}); err != nil {
		_ = cl.AbortMultipart(context.WithoutCancel(ctx), objectKey(sc, s.Version), s.UploadID)
		e.storageError(c, err)
		return
	}
	parts, err := e.partURLs(ctx, sc, cl, s, nil)
	if err != nil {
		e.storageError(c, storeErr(err))
		return
	}
	c.json(http.StatusOK, map[string]any{"id": s.ID, "bucket": bucket, "path": path, "size": s.Size, "part_size": s.PartSize,
		"parts": parts, "expires_at": s.ExpiresAt.UTC()})
}

type partURL struct {
	Number int32  `json:"number"`
	Size   int64  `json:"size"`
	URL    string `json:"url"`
}

// partURLs are presigned URLs for the parts not in done.
func (e *Edge) partURLs(ctx context.Context, sc *edgeapi.StorageConfig, cl *storage.Client, s *session, done map[int32]bool) ([]partURL, error) {
	ttl := time.Until(s.ExpiresAt)
	out := []partURL{}
	for n := int32(1); n <= s.parts(); n++ {
		if done[n] {
			continue
		}
		// Each URL takes exactly its part: the last one the remainder.
		size := s.PartSize
		if n == s.parts() {
			size = s.Size - int64(n-1)*s.PartSize
		}
		u, err := cl.PresignPart(ctx, objectKey(sc, s.Version), s.UploadID, n, size, ttl)
		if err != nil {
			return nil, err
		}
		out = append(out, partURL{Number: n, Size: size, URL: u})
	}
	return out, nil
}

var errNoUpload = refuse(http.StatusNotFound, "upload_not_found", "no such upload, or it was completed, aborted or has expired")

func (e *Edge) loadSession(ctx context.Context, p *project, id uuid.UUID) (*session, error) {
	pool, err := e.dbPool(ctx, p)
	if err != nil {
		return nil, err
	}
	s := &session{ID: id}
	var claims []byte
	err = pool.QueryRow(ctx, `SELECT bucket, path, version, upload_id, size, part_size, mime_type, upsert, owner, role, claims,
		user_metadata, expires_at FROM xb_storage.uploads WHERE id = $1`, id).Scan(&s.Bucket, &s.Path, &s.Version, &s.UploadID, &s.Size,
		&s.PartSize, &s.MIMEType, &s.Upsert, &s.Owner, &s.Role, &claims, &s.Metadata, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && time.Now().After(s.ExpiresAt)) {
		return nil, errNoUpload
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(claims, &s.Claims); err != nil {
		return nil, err
	}
	return s, nil
}

func (e *Edge) dropSession(ctx context.Context, p *project, id uuid.UUID) error {
	pool, err := e.dbPool(ctx, p)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `DELETE FROM xb_storage.uploads WHERE id = $1`, id)
	return err
}

// uploadStatus lists the parts uploaded and fresh URLs for the rest (to
// resume).
func (e *Edge) uploadStatus(c *call, id uuid.UUID) {
	sc, cl, ok := e.store(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), 30*time.Second)
	defer cancel()
	s, err := e.loadSession(ctx, c.p, id)
	if err != nil {
		e.storageError(c, err)
		return
	}
	got, err := cl.Parts(ctx, objectKey(sc, s.Version), s.UploadID)
	if errors.Is(err, storage.ErrNotFound) {
		errNoUpload.send(c)
		return
	}
	if err != nil {
		e.storageError(c, storeErr(err))
		return
	}
	done := map[int32]bool{}
	uploaded := []map[string]any{}
	for _, p := range got {
		done[p.Number] = true
		uploaded = append(uploaded, map[string]any{"number": p.Number, "size": p.Size})
	}
	missing, err := e.partURLs(ctx, sc, cl, s, done)
	if err != nil {
		e.storageError(c, storeErr(err))
		return
	}
	c.json(http.StatusOK, map[string]any{"id": s.ID, "bucket": s.Bucket, "path": s.Path, "size": s.Size, "part_size": s.PartSize,
		"uploaded": uploaded, "parts": missing, "expires_at": s.ExpiresAt.UTC()})
}

// completeUpload joins the parts and writes the row as the caller that
// started the upload.
func (e *Edge) completeUpload(c *call, id uuid.UUID) {
	sc, cl, ok := e.store(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), 10*time.Minute)
	defer cancel()
	s, err := e.loadSession(ctx, c.p, id)
	if err != nil {
		e.storageError(c, err)
		return
	}
	key := objectKey(sc, s.Version)
	parts, err := cl.Parts(ctx, key, s.UploadID)
	if errors.Is(err, storage.ErrNotFound) {
		errNoUpload.send(c)
		return
	}
	if err != nil {
		e.storageError(c, storeErr(err))
		return
	}
	var total int64
	for i, p := range parts {
		if p.Number != int32(i+1) {
			c.fail(http.StatusBadRequest, "upload_incomplete", fmt.Sprintf("part %d hasn't been uploaded", i+1))
			return
		}
		total += p.Size
	}
	if int32(len(parts)) != s.parts() || total != s.Size {
		c.fail(http.StatusBadRequest, "upload_incomplete", fmt.Sprintf("%d of %d bytes in %d of %d parts are uploaded", total, s.Size, len(parts), s.parts()))
		return
	}
	etag, err := cl.CompleteMultipart(ctx, key, s.UploadID, parts)
	if err != nil {
		e.storageError(c, storeErr(err))
		return
	}
	// The declared type against what the first bytes are.
	if head, err := cl.Get(ctx, key, "bytes=0-511"); err == nil {
		b, _ := io.ReadAll(bufio.NewReader(io.LimitReader(head.Body, 512)))
		_ = head.Body.Close()
		if _, err := mimeFor(s.MIMEType, b); err != nil {
			_ = cl.Delete(context.WithoutCancel(ctx), key)
			_ = e.dropSession(context.WithoutCancel(ctx), c.p, id)
			e.storageError(c, err)
			return
		}
	}
	req := Request{Role: s.Role, Claims: s.Claims, Timeout: time.Duration(c.p.cfg.Settings.StatementTimeoutMs) * time.Millisecond}
	u := upload{bucket: s.Bucket, path: s.Path, version: s.Version, size: s.Size, mime: s.MIMEType, etag: etag, owner: s.Owner,
		metadata: s.Metadata, upsert: s.Upsert}
	o, err := e.commit(ctx, c, req, sc, cl, u, nil)
	_ = e.dropSession(context.WithoutCancel(ctx), c.p, id)
	if err != nil {
		e.storageError(c, err)
		return
	}
	c.json(http.StatusOK, map[string]any{"id": o.ID, "key": o.Bucket + "/" + o.Path, "bucket": o.Bucket, "path": o.Path,
		"size": o.Size, "mime_type": o.MIMEType, "etag": o.ETag})
}

// abortUpload abandons an upload and its parts.
func (e *Edge) abortUpload(c *call, id uuid.UUID) {
	sc, cl, ok := e.store(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), 30*time.Second)
	defer cancel()
	s, err := e.loadSession(ctx, c.p, id)
	if err != nil {
		e.storageError(c, err)
		return
	}
	if err := cl.AbortMultipart(ctx, objectKey(sc, s.Version), s.UploadID); err != nil {
		e.storageError(c, storeErr(err))
		return
	}
	if err := e.dropSession(ctx, c.p, id); err != nil {
		e.dbError(c, err)
		return
	}
	c.json(http.StatusOK, map[string]string{"message": "aborted"})
}
