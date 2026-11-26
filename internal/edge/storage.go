package edge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/files"
	"github.com/israel-duff/xenibase/internal/storage"
)

// Storage (V4 §5): buckets and objects' metadata are rows in the project's
// xb_storage schema, read and written as the caller's role so the
// project's row-level security policies decide every access; the bytes are
// in the region's object store under the object's version, which no caller
// chooses.

// directMax is the largest upload through the edge; larger ones go to the
// object store with presigned multipart URLs (V4 §5.3).
const directMax = 50 << 20

// fileStore is a project's object store client, kept across configuration
// copies while the store doesn't change.
type fileStore struct {
	mu     sync.Mutex
	target storage.Target
	client *storage.Client
	// collecting is set while a clean-up of replaced and deleted bytes is
	// scheduled.
	collecting bool
}

func sameStore(a, b *edgeapi.StorageConfig) bool {
	return a != nil && b != nil && a.Target == b.Target && a.Prefix == b.Prefix
}

// store is the project's storage configuration and client; false means c
// was answered.
func (e *Edge) store(c *call) (*edgeapi.StorageConfig, *storage.Client, bool) {
	sc := c.p.cfg.Storage
	if sc == nil || c.p.files == nil {
		c.fail(http.StatusServiceUnavailable, "storage_unavailable", "file storage isn't available for this project's region")
		return nil, nil, false
	}
	cl, err := c.p.files.get(sc)
	if err != nil {
		e.cfg.Log.Warn("edge object store", "project", c.p.cfg.Ref, "err", err)
		c.fail(http.StatusServiceUnavailable, "storage_unavailable", "the project's object store isn't usable right now")
		return nil, nil, false
	}
	return sc, cl, true
}

// get is the client for sc's store, made the first time.
func (f *fileStore) get(sc *edgeapi.StorageConfig) (*storage.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.client == nil || f.target != sc.Target {
		cl, err := storage.New(sc.Target)
		if err != nil {
			return nil, err
		}
		f.client, f.target = cl, sc.Target
	}
	return f.client, nil
}

// objectKey is where an object version's bytes are.
func objectKey(sc *edgeapi.StorageConfig, version uuid.UUID) string {
	return files.ObjectKey(sc.Prefix, version)
}

// splitBucketPath parses "bucket/path..." (the rest of a URL path).
func splitBucketPath(rest string) (string, string, bool) {
	b, p, ok := strings.Cut(rest, "/")
	if !ok || !files.BucketRe.MatchString(b) || !files.ValidPath(p) {
		return "", "", false
	}
	return b, p, true
}

// ---- Buckets -------------------------------------------------------------------

// Bucket is a bucket as the API shows it.
type Bucket struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Public           bool      `json:"public"`
	FileSizeLimit    *int64    `json:"file_size_limit"`
	AllowedMIMETypes []string  `json:"allowed_mime_types"`
	CacheSeconds     int       `json:"cache_seconds"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func scanBucket(row pgx.Row) (*Bucket, error) {
	var b Bucket
	if err := row.Scan(&b.ID, &b.Public, &b.FileSizeLimit, &b.AllowedMIMETypes, &b.CacheSeconds, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return nil, err
	}
	b.Name = b.ID
	if b.AllowedMIMETypes == nil {
		b.AllowedMIMETypes = []string{}
	}
	return &b, nil
}

var errNoBucket = refuse(http.StatusNotFound, "bucket_not_found", "no such bucket")

// bucket loads a bucket with the edge's own login.
func (e *Edge) bucket(ctx context.Context, p *project, id string) (*Bucket, error) {
	pool, err := e.dbPool(ctx, p)
	if err != nil {
		return nil, err
	}
	b, err := scanBucket(pool.QueryRow(ctx, `SELECT `+files.BucketCols+` FROM xb_storage.buckets WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoBucket
	}
	return b, err
}

// bucketInput is a bucket's settings in a create or update.
type bucketInput struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Public           *bool     `json:"public"`
	FileSizeLimit    *int64    `json:"file_size_limit"`
	AllowedMIMETypes *[]string `json:"allowed_mime_types"`
	CacheSeconds     *int      `json:"cache_seconds"`
}

func (in bucketInput) check() *apiErr {
	if in.FileSizeLimit != nil && *in.FileSizeLimit <= 0 {
		return refuse(http.StatusBadRequest, "invalid_bucket", "file_size_limit must be a positive number of bytes")
	}
	if in.CacheSeconds != nil && (*in.CacheSeconds < 0 || *in.CacheSeconds > 31536000) {
		return refuse(http.StatusBadRequest, "invalid_bucket", "cache_seconds must be between 0 and a year")
	}
	if in.AllowedMIMETypes != nil {
		if len(*in.AllowedMIMETypes) > 100 {
			return refuse(http.StatusBadRequest, "invalid_bucket", "at most 100 allowed MIME types")
		}
		for _, m := range *in.AllowedMIMETypes {
			if !files.MIMEPatternRe.MatchString(m) {
				return refuse(http.StatusBadRequest, "invalid_bucket", "allowed_mime_types holds types like image/png or image/*")
			}
		}
	}
	return nil
}

// buckets serves /storage/v1/bucket[/{id}[/empty]], for the secret key.
func (e *Edge) buckets(c *call, req Request, rest string) {
	if req.Role != "service" {
		c.fail(http.StatusForbidden, "secret_key_required", "managing buckets needs the project's secret key")
		return
	}
	ctx, cancel := context.WithTimeout(c.r.Context(), 30*time.Second)
	defer cancel()
	pool, err := e.dbPool(ctx, c.p)
	if err != nil {
		e.dbError(c, err)
		return
	}
	id, sub, _ := strings.Cut(strings.Trim(rest, "/"), "/")
	switch {
	case id == "" && c.r.Method == http.MethodGet:
		rows, err := pool.Query(ctx, `SELECT `+files.BucketCols+` FROM xb_storage.buckets ORDER BY id`)
		if err != nil {
			e.dbError(c, err)
			return
		}
		out := []*Bucket{}
		for rows.Next() {
			b, err := scanBucket(rows)
			if err != nil {
				rows.Close()
				e.dbError(c, err)
				return
			}
			out = append(out, b)
		}
		if err := rows.Err(); err != nil {
			e.dbError(c, err)
			return
		}
		c.json(http.StatusOK, out)
	case id == "" && c.r.Method == http.MethodPost:
		var in bucketInput
		if !authBody(c, &in) {
			return
		}
		if in.ID == "" {
			in.ID = in.Name
		}
		if !files.ValidBucket(in.ID) {
			c.fail(http.StatusBadRequest, "invalid_bucket", files.BucketRule)
			return
		}
		if a := in.check(); a != nil {
			a.send(c)
			return
		}
		cache := 3600
		if in.CacheSeconds != nil {
			cache = *in.CacheSeconds
		}
		var mimes []string
		if in.AllowedMIMETypes != nil && len(*in.AllowedMIMETypes) > 0 {
			mimes = *in.AllowedMIMETypes
		}
		b, err := scanBucket(pool.QueryRow(ctx, `INSERT INTO xb_storage.buckets (id, public, file_size_limit, allowed_mime_types, cache_seconds)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+files.BucketCols, in.ID, in.Public != nil && *in.Public, in.FileSizeLimit, mimes, cache))
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			c.fail(http.StatusConflict, "bucket_exists", "a bucket with this id already exists")
			return
		}
		if err != nil {
			e.dbError(c, err)
			return
		}
		c.json(http.StatusCreated, b)
	case id != "" && sub == "" && c.r.Method == http.MethodGet:
		b, err := e.bucket(ctx, c.p, id)
		if err != nil {
			e.storageError(c, err)
			return
		}
		c.json(http.StatusOK, b)
	case id != "" && sub == "" && (c.r.Method == http.MethodPut || c.r.Method == http.MethodPatch):
		var in bucketInput
		if !authBody(c, &in) {
			return
		}
		if a := in.check(); a != nil {
			a.send(c)
			return
		}
		old, err := e.bucket(ctx, c.p, id)
		if err != nil {
			e.storageError(c, err)
			return
		}
		public, limit, mimes, cache := old.Public, old.FileSizeLimit, old.AllowedMIMETypes, old.CacheSeconds
		if in.Public != nil {
			public = *in.Public
		}
		if in.FileSizeLimit != nil {
			limit = in.FileSizeLimit
		}
		if in.AllowedMIMETypes != nil {
			mimes = *in.AllowedMIMETypes
		}
		if len(mimes) == 0 {
			mimes = nil
		}
		if in.CacheSeconds != nil {
			cache = *in.CacheSeconds
		}
		b, err := scanBucket(pool.QueryRow(ctx, `UPDATE xb_storage.buckets SET public = $2, file_size_limit = $3, allowed_mime_types = $4,
			cache_seconds = $5, updated_at = now() WHERE id = $1 RETURNING `+files.BucketCols, id, public, limit, mimes, cache))
		if errors.Is(err, pgx.ErrNoRows) {
			errNoBucket.send(c)
			return
		}
		if err != nil {
			e.dbError(c, err)
			return
		}
		if old.Public && !b.Public {
			e.storageEvent(c.p, edgeapi.StorageEvent{Event: edgeapi.EventBucketPrivate, Bucket: id})
		}
		c.json(http.StatusOK, b)
	case id != "" && sub == "" && c.r.Method == http.MethodDelete:
		var public bool
		err := pool.QueryRow(ctx, `DELETE FROM xb_storage.buckets WHERE id = $1 RETURNING public`, id).Scan(&public)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23503" {
			c.fail(http.StatusConflict, "bucket_not_empty", "the bucket still has objects: empty it first")
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			errNoBucket.send(c)
			return
		}
		if err != nil {
			e.dbError(c, err)
			return
		}
		if public {
			e.storageEvent(c.p, edgeapi.StorageEvent{Event: edgeapi.EventBucketDeleted, Bucket: id})
		}
		c.json(http.StatusOK, map[string]string{"message": "deleted"})
	case id != "" && sub == "empty" && c.r.Method == http.MethodPost:
		b, err := e.bucket(ctx, c.p, id)
		if err != nil {
			e.storageError(c, err)
			return
		}
		var n int64
		for {
			tag, err := pool.Exec(ctx, `DELETE FROM xb_storage.objects WHERE id IN
				(SELECT id FROM xb_storage.objects WHERE bucket = $1 LIMIT 1000)`, id)
			if err != nil {
				e.dbError(c, err)
				return
			}
			n += tag.RowsAffected()
			if tag.RowsAffected() == 0 {
				break
			}
		}
		e.collectSoon(c.p)
		if b.Public && n > 0 {
			e.storageEvent(c.p, edgeapi.StorageEvent{Event: edgeapi.EventBucketEmptied, Bucket: id})
		}
		c.json(http.StatusOK, map[string]any{"message": "emptied", "deleted": n})
	default:
		c.fail(http.StatusNotFound, "no_such_endpoint", "no such endpoint")
	}
}

// storageEvent tells xenibase-server, in the background, that files left
// a public bucket's URLs, so they leave the CDN's cache (V4 §5.4); the
// server keeps the purge until the CDN takes it.
func (e *Edge) storageEvent(p *project, ev edgeapi.StorageEvent) {
	ev.Ref = p.cfg.Ref
	go func() {
		var err error
		for i := range 3 {
			if i > 0 {
				time.Sleep(time.Duration(i) * 2 * time.Second)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = e.client.StorageEvent(ctx, ev)
			cancel()
			if err == nil {
				return
			}
		}
		e.cfg.Log.Warn("storage event", "project", ev.Ref, "event", ev.Event, "bucket", ev.Bucket, "err", err)
	}()
}

// publicGone sends paths of bucket b, gone from its public URLs, to have
// them purged; nothing for a private bucket.
func (e *Edge) publicGone(p *project, b *Bucket, paths ...string) {
	if b != nil && b.Public && len(paths) > 0 {
		e.storageEvent(p, edgeapi.StorageEvent{Event: edgeapi.EventObjectsChanged, Bucket: b.ID, Paths: paths})
	}
}

// storageError answers err from a storage operation.
func (e *Edge) storageError(c *call, err error) {
	var a *apiErr
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &a):
		a.send(c)
	case errors.As(err, &pe) && pe.Code == "42501":
		// Row-level security refused the write (V4 §5.1).
		c.fail(http.StatusForbidden, "not_allowed", "the project's storage policies don't allow this")
	case errors.As(err, &pe) && pe.Code == "23505":
		c.fail(http.StatusConflict, "already_exists", "an object already exists at this path")
	case errors.As(err, &pe) && pe.Code == "23503":
		errNoBucket.send(c)
	case errors.Is(err, storage.ErrNotFound):
		c.fail(http.StatusNotFound, "object_not_found", "no such object")
	case errors.Is(err, errObjectStore):
		e.cfg.Log.Warn("edge object store", "project", c.p.cfg.Ref, "err", err)
		c.w.Header().Set("Retry-After", "5")
		c.fail(http.StatusServiceUnavailable, "storage_unavailable", "the project's object store can't be reached right now")
	case errors.As(err, &pe):
		e.dbError(c, err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		c.fail(http.StatusGatewayTimeout, "timeout", "the request took too long")
	default:
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			c.fail(http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
		e.dbError(c, err)
	}
}
