package services

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/israel-duff/xenibase/internal/files"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
)

// The dashboard's storage (V4 §5): buckets and files, managed as the
// platform (no policies apply), through the same rules as xenibase-edge.

// DashboardUploadMax is the largest file the dashboard uploads; bigger ones
// go through the API's multipart uploads.
const DashboardUploadMax = 50 << 20

// Bucket is a bucket with what it holds.
type Bucket struct {
	ID               string
	Public           bool
	FileSizeLimit    *int64
	AllowedMIMETypes []string
	CacheSeconds     int
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Objects, Bytes   int64
}

// StorageOverview is a project's storage page.
type StorageOverview struct {
	Bytes, Objects    int64
	QuotaBytes        *int64
	UploadMaxBytes    int64
	EgressBlocked     bool
	TransformsBlocked bool
	MeasuredAt        *time.Time
	MissingObjects    int32
	MissingSample     []string
	ReconciledAt      *time.Time
	Buckets           []Bucket
}

// FileObject is a stored file's metadata.
type FileObject struct {
	ID        uuid.UUID
	Bucket    string
	Path      string
	Version   uuid.UUID
	Size      int64
	MIMEType  string
	ETag      string
	Checksum  *string
	Owner     *uuid.UUID
	Metadata  json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

func scanFile(row pgx.Row) (*FileObject, error) {
	var o FileObject
	if err := row.Scan(&o.ID, &o.Bucket, &o.Path, &o.Version, &o.Size, &o.MIMEType, &o.ETag, &o.Checksum, &o.Owner, &o.Metadata,
		&o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, err
	}
	return &o, nil
}

// FileEntry is a listed name: a file, or a folder.
type FileEntry struct {
	Name   string
	Path   string
	Folder bool
	Object *FileObject
}

// storageCtx is what a dashboard storage call works with.
type storageCtx struct {
	p      store.Project
	svc    store.ProjectService
	conn   *pgx.Conn
	client *storage.Client
}

func (sc *storageCtx) close() { sc.conn.Close(context.Background()) }

func (sc *storageCtx) prefix() string { return files.Prefix(sc.svc.Ref) }

func (s *Service) openStorage(ctx context.Context, projectID uuid.UUID) (*storageCtx, error) {
	q := store.New(s.db)
	p, err := q.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	svc, err := q.GetProjectServices(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !svc.Enabled) {
		return nil, fmt.Errorf("%w: backend services aren't enabled for this project", ErrConflict)
	}
	if err != nil {
		return nil, err
	}
	if svc.SchemaVersion < 4 {
		return nil, fmt.Errorf("%w: the project's storage schema isn't ready yet; try again in a minute", ErrConflict)
	}
	cl, err := s.filesClient(ctx, p.Region, p.DataResidency)
	if err != nil {
		return nil, fmt.Errorf("%w: file storage isn't available in this project's region: %w", ErrConflict, err)
	}
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return nil, err
	}
	return &storageCtx{p: p, svc: svc, conn: conn, client: cl}, nil
}

// Storage is a project's storage page: usage, limits, the reconciler's
// findings and the buckets.
func (s *Service) Storage(ctx context.Context, projectID uuid.UUID) (StorageOverview, error) {
	var out StorageOverview
	sc, err := s.openStorage(ctx, projectID)
	if err != nil {
		return out, err
	}
	defer sc.close()
	out.QuotaBytes, out.UploadMaxBytes = sc.svc.StorageQuotaBytes, DefaultUploadMax
	if sc.svc.UploadMaxBytes != nil {
		out.UploadMaxBytes = *sc.svc.UploadMaxBytes
	}
	out.EgressBlocked, out.TransformsBlocked = sc.svc.StorageEgressBlocked, sc.svc.TransformsBlocked
	if ps, err := store.New(s.db).GetProjectStorage(ctx, projectID); err == nil {
		out.MeasuredAt, out.MissingObjects, out.MissingSample, out.ReconciledAt = ps.MeasuredAt, ps.MissingObjects, ps.MissingSample, ps.ReconciledAt
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if err := sc.conn.QueryRow(ctx, `SELECT bytes, objects FROM xb_storage.usage`).Scan(&out.Bytes, &out.Objects); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	rows, err := sc.conn.Query(ctx, `SELECT b.id, b.public, b.file_size_limit, b.allowed_mime_types, b.cache_seconds, b.created_at, b.updated_at,
		count(o.id), coalesce(sum(o.size), 0)::bigint
		FROM xb_storage.buckets b LEFT JOIN xb_storage.objects o ON o.bucket = b.id GROUP BY b.id ORDER BY b.id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Buckets = []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.ID, &b.Public, &b.FileSizeLimit, &b.AllowedMIMETypes, &b.CacheSeconds, &b.CreatedAt, &b.UpdatedAt, &b.Objects, &b.Bytes); err != nil {
			return out, err
		}
		if b.AllowedMIMETypes == nil {
			b.AllowedMIMETypes = []string{}
		}
		out.Buckets = append(out.Buckets, b)
	}
	if out.MissingSample == nil {
		out.MissingSample = []string{}
	}
	return out, rows.Err()
}

// BucketInput is a bucket's settings; nil fields keep theirs (or the
// defaults). Unlimit clears the size limit.
type BucketInput struct {
	Public           *bool
	FileSizeLimit    *int64
	Unlimit          bool
	AllowedMIMETypes *[]string
	CacheSeconds     *int
}

func (in BucketInput) check() error {
	if in.FileSizeLimit != nil && *in.FileSizeLimit <= 0 {
		return fmt.Errorf("%w: the file size limit must be a positive number of bytes", ErrInvalid)
	}
	if in.CacheSeconds != nil && (*in.CacheSeconds < 0 || *in.CacheSeconds > 31536000) {
		return fmt.Errorf("%w: the cache time must be between 0 and a year", ErrInvalid)
	}
	if in.AllowedMIMETypes != nil {
		if len(*in.AllowedMIMETypes) > 100 {
			return fmt.Errorf("%w: at most 100 allowed MIME types", ErrInvalid)
		}
		for _, m := range *in.AllowedMIMETypes {
			if !files.MIMEPatternRe.MatchString(m) {
				return fmt.Errorf("%w: allowed MIME types are like image/png or image/*", ErrInvalid)
			}
		}
	}
	return nil
}

func (s *Service) bucketInfo(ctx context.Context, conn *pgx.Conn, id string) (Bucket, error) {
	var b Bucket
	err := conn.QueryRow(ctx, `SELECT b.id, b.public, b.file_size_limit, b.allowed_mime_types, b.cache_seconds, b.created_at, b.updated_at,
		(SELECT count(*) FROM xb_storage.objects o WHERE o.bucket = b.id), (SELECT coalesce(sum(size), 0)::bigint FROM xb_storage.objects o WHERE o.bucket = b.id)
		FROM xb_storage.buckets b WHERE b.id = $1`, id).Scan(&b.ID, &b.Public, &b.FileSizeLimit, &b.AllowedMIMETypes, &b.CacheSeconds,
		&b.CreatedAt, &b.UpdatedAt, &b.Objects, &b.Bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, fmt.Errorf("%w: no bucket %s", ErrNotFound, id)
	}
	if b.AllowedMIMETypes == nil {
		b.AllowedMIMETypes = []string{}
	}
	return b, err
}

// CreateBucket makes a bucket.
func (s *Service) CreateBucket(ctx context.Context, projectID uuid.UUID, id string, in BucketInput) (Bucket, error) {
	if !files.ValidBucket(id) {
		return Bucket{}, fmt.Errorf("%w: %s", ErrInvalid, files.BucketRule)
	}
	if err := in.check(); err != nil {
		return Bucket{}, err
	}
	sc, err := s.openStorage(ctx, projectID)
	if err != nil {
		return Bucket{}, err
	}
	defer sc.close()
	cache := 3600
	if in.CacheSeconds != nil {
		cache = *in.CacheSeconds
	}
	var mimes []string
	if in.AllowedMIMETypes != nil && len(*in.AllowedMIMETypes) > 0 {
		mimes = *in.AllowedMIMETypes
	}
	_, err = sc.conn.Exec(ctx, `INSERT INTO xb_storage.buckets (id, public, file_size_limit, allowed_mime_types, cache_seconds) VALUES ($1, $2, $3, $4, $5)`,
		id, in.Public != nil && *in.Public, in.FileSizeLimit, mimes, cache)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return Bucket{}, fmt.Errorf("%w: a bucket %s already exists", ErrConflict, id)
	}
	if err != nil {
		return Bucket{}, err
	}
	return s.bucketInfo(ctx, sc.conn, id)
}

// UpdateBucket changes a bucket's settings; one made private leaves the
// CDN's cache.
func (s *Service) UpdateBucket(ctx context.Context, projectID uuid.UUID, id string, in BucketInput) (Bucket, error) {
	if err := in.check(); err != nil {
		return Bucket{}, err
	}
	sc, err := s.openStorage(ctx, projectID)
	if err != nil {
		return Bucket{}, err
	}
	defer sc.close()
	old, err := s.bucketInfo(ctx, sc.conn, id)
	if err != nil {
		return Bucket{}, err
	}
	public, limit, mimes, cache := old.Public, old.FileSizeLimit, old.AllowedMIMETypes, old.CacheSeconds
	if in.Public != nil {
		public = *in.Public
	}
	if in.FileSizeLimit != nil {
		limit = in.FileSizeLimit
	}
	if in.Unlimit {
		limit = nil
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
	if _, err := sc.conn.Exec(ctx, `UPDATE xb_storage.buckets SET public = $2, file_size_limit = $3, allowed_mime_types = $4, cache_seconds = $5,
		updated_at = now() WHERE id = $1`, id, public, limit, mimes, cache); err != nil {
		return Bucket{}, err
	}
	if old.Public && !public {
		if err := s.PurgeBucket(ctx, projectID, sc.svc.Ref, sc.p.Region, id); err != nil {
			s.log.Warn("purge a bucket made private from the CDN", "project_id", projectID, "bucket", id, "err", err)
		}
	}
	return s.bucketInfo(ctx, sc.conn, id)
}

// DeleteBucket removes an empty bucket, or with empty its files first.
func (s *Service) DeleteBucket(ctx context.Context, projectID uuid.UUID, id string, empty bool) error {
	sc, err := s.openStorage(ctx, projectID)
	if err != nil {
		return err
	}
	defer sc.close()
	if empty {
		if _, err := sc.conn.Exec(ctx, `DELETE FROM xb_storage.objects WHERE bucket = $1`, id); err != nil {
			return err
		}
	}
	var public bool
	err = sc.conn.QueryRow(ctx, `DELETE FROM xb_storage.buckets WHERE id = $1 RETURNING public`, id).Scan(&public)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" {
		return fmt.Errorf("%w: the bucket still has files; empty it first", ErrConflict)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no bucket %s", ErrNotFound, id)
	}
	if err != nil {
		return err
	}
	if public {
		if err := s.PurgeBucket(ctx, projectID, sc.svc.Ref, sc.p.Region, id); err != nil {
			s.log.Warn("purge a deleted public bucket from the CDN", "project_id", projectID, "bucket", id, "err", err)
		}
	}
	return nil
}

// ListFiles lists a bucket's folder (the names directly under prefix),
// after a cursor.
func (s *Service) ListFiles(ctx context.Context, projectID uuid.UUID, bucket, prefix, cursor string, limit int) ([]FileEntry, string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	prefix = strings.TrimLeft(prefix, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if len(prefix) > files.MaxPath || !utf8.ValidString(prefix) {
		return nil, "", fmt.Errorf("%w: the prefix isn't a valid path", ErrInvalid)
	}
	sc, err := s.openStorage(ctx, projectID)
	if err != nil {
		return nil, "", err
	}
	defer sc.close()
	if _, err := s.bucketInfo(ctx, sc.conn, bucket); err != nil {
		return nil, "", err
	}
	curName, curFolder := "", false
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		var cur struct {
			N string `json:"n"`
			F bool   `json:"f"`
		}
		if err != nil || json.Unmarshal(raw, &cur) != nil {
			return nil, "", fmt.Errorf("%w: the cursor isn't one this API gave", ErrInvalid)
		}
		curName, curFolder = cur.N, cur.F
	}
	rows, err := sc.conn.Query(ctx, files.ListFolderSQL, bucket, files.LikePrefix(prefix), utf8.RuneCountInString(prefix)+1, curName, curFolder, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []FileEntry{}
	for rows.Next() {
		var en FileEntry
		var id, version, owner *uuid.UUID
		var size *int64
		var mt, etag *string
		var md json.RawMessage
		var created, updated *time.Time
		if err := rows.Scan(&en.Name, &en.Folder, &id, &version, &size, &mt, &etag, &owner, &md, &created, &updated); err != nil {
			return nil, "", err
		}
		en.Path = prefix + en.Name
		if !en.Folder && id != nil {
			en.Object = &FileObject{ID: *id, Bucket: bucket, Path: en.Path, Version: *version, Size: *size, MIMEType: *mt, ETag: *etag,
				Owner: owner, Metadata: md, CreatedAt: *created, UpdatedAt: *updated}
		}
		out = append(out, en)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		raw, _ := json.Marshal(map[string]any{"n": last.Name, "f": last.Folder})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, next, nil
}

// UploadFile stores a file (replacing one at the same path) up to
// DashboardUploadMax, under the bucket's and the plan's limits.
func (s *Service) UploadFile(ctx context.Context, projectID uuid.UUID, bucket, path, contentType string, body io.Reader) (*FileObject, error) {
	if !files.ValidPath(path) {
		return nil, fmt.Errorf("%w: not a valid file path", ErrInvalid)
	}
	sc, err := s.openStorage(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer sc.close()
	b, err := s.bucketInfo(ctx, sc.conn, bucket)
	if err != nil {
		return nil, err
	}
	limit := int64(DashboardUploadMax)
	if sc.svc.UploadMaxBytes != nil {
		limit = min(limit, *sc.svc.UploadMaxBytes)
	}
	if b.FileSizeLimit != nil {
		limit = min(limit, *b.FileSizeLimit)
	}
	br := bufio.NewReaderSize(body, 512)
	head, _ := br.Peek(512)
	mt, err := files.DetectMIME(contentType, head)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if !files.MIMEAllowed(b.AllowedMIMETypes, mt) {
		return nil, fmt.Errorf("%w: this bucket doesn't take %s files", ErrInvalid, mt)
	}
	// Spooled, so the store gets a length.
	tmp, n, err := spoolTemp(br, limit)
	if err != nil {
		return nil, err
	}
	defer tmp.close()
	if q := sc.svc.StorageQuotaBytes; q != nil {
		var used int64
		// Large uploads in progress hold what they declared (as at the edge).
		if err := sc.conn.QueryRow(ctx, `SELECT coalesce((SELECT bytes FROM xb_storage.usage), 0)
			+ coalesce((SELECT sum(size) FROM xb_storage.uploads WHERE expires_at > now()), 0)::bigint`).Scan(&used); err != nil {
			return nil, err
		}
		if used+n > *q {
			return nil, fmt.Errorf("%w: the organisation's file storage quota is used up", ErrConflict)
		}
	}
	version := uuid.New()
	h := sha256.New()
	etag, err := sc.client.Put(ctx, files.ObjectKey(sc.prefix(), version), &hashing{r: tmp.f, h: h}, n, mt)
	if err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	o, err := scanFile(sc.conn.QueryRow(ctx, files.InsertSQL(true), bucket, path, version, n, mt, etag, &sum, nil, json.RawMessage(`{}`)))
	if err != nil {
		_ = sc.client.Delete(context.WithoutCancel(ctx), files.ObjectKey(sc.prefix(), version))
		return nil, err
	}
	// A replaced public file leaves the CDN's cache (an upsert that
	// replaced a row keeps its created_at).
	if b.Public && o.CreatedAt.Before(o.UpdatedAt) {
		if err := s.PurgeObjects(ctx, projectID, sc.svc.Ref, sc.p.Region, bucket, []string{path}); err != nil {
			s.log.Warn("purge a replaced public file from the CDN", "project_id", projectID, "bucket", bucket, "err", err)
		}
	}
	return o, nil
}

// trimmed is paths without a folder's trailing slash.
func trimmed(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, strings.TrimSuffix(p, "/"))
	}
	return out
}

type hashing struct {
	r io.Reader
	h hash.Hash
}

func (x *hashing) Read(p []byte) (int, error) {
	n, err := x.r.Read(p)
	x.h.Write(p[:n])
	return n, err
}

// OpenFile is a file's metadata and bytes; the caller closes the body.
func (s *Service) OpenFile(ctx context.Context, projectID uuid.UUID, bucket, path string) (*FileObject, io.ReadCloser, error) {
	sc, err := s.openStorage(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	defer sc.close()
	o, err := scanFile(sc.conn.QueryRow(ctx, `SELECT `+files.ObjectCols+` FROM xb_storage.objects WHERE bucket = $1 AND path = $2`, bucket, path))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("%w: no such file", ErrNotFound)
	}
	if err != nil {
		return nil, nil, err
	}
	obj, err := sc.client.Get(ctx, files.ObjectKey(sc.prefix(), o.Version), "")
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil, fmt.Errorf("%w: the file's data is missing", ErrNotFound)
	}
	if err != nil {
		return nil, nil, err
	}
	return o, obj.Body, nil
}

// DeleteFiles removes files (their bytes go with the next sweep); it
// returns how many there were.
func (s *Service) DeleteFiles(ctx context.Context, projectID uuid.UUID, bucket string, paths []string) (int64, error) {
	if len(paths) == 0 || len(paths) > 1000 {
		return 0, fmt.Errorf("%w: name 1–1000 files", ErrInvalid)
	}
	sc, err := s.openStorage(ctx, projectID)
	if err != nil {
		return 0, err
	}
	defer sc.close()
	b, err := s.bucketInfo(ctx, sc.conn, bucket)
	if err != nil {
		return 0, err
	}
	var n int64
	// A public bucket's deleted files leave the CDN's cache (a folder's
	// path is a prefix of its files').
	defer func() {
		if b.Public && n > 0 {
			if err := s.PurgeObjects(ctx, projectID, sc.svc.Ref, sc.p.Region, bucket, trimmed(paths)); err != nil {
				s.log.Warn("purge deleted public files from the CDN", "project_id", projectID, "bucket", bucket, "err", err)
			}
		}
	}()
	// A folder's path takes everything under it.
	for _, p := range paths {
		tag, err := sc.conn.Exec(ctx, `DELETE FROM xb_storage.objects WHERE bucket = $1 AND (path = $2 OR path LIKE $3 ESCAPE '\')`,
			bucket, strings.TrimSuffix(p, "/"), files.LikePrefix(strings.TrimSuffix(p, "/")+"/"))
		if err != nil {
			return n, err
		}
		n += tag.RowsAffected()
	}
	return n, nil
}

// SignFile makes a signed download URL for a file.
func (s *Service) SignFile(ctx context.Context, projectID uuid.UUID, bucket, path string, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	ttl = min(ttl, 7*24*time.Hour)
	sc, err := s.openStorage(ctx, projectID)
	if err != nil {
		return "", time.Time{}, err
	}
	defer sc.close()
	var exists bool
	if err := sc.conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM xb_storage.objects WHERE bucket = $1 AND path = $2)`, bucket, path).Scan(&exists); err != nil {
		return "", time.Time{}, err
	}
	if !exists {
		return "", time.Time{}, fmt.Errorf("%w: no such file", ErrNotFound)
	}
	base := s.URL(sc.svc.Ref, sc.p.Region)
	if base == "" {
		return "", time.Time{}, fmt.Errorf("%w: backend services have no API domain on this install", ErrConflict)
	}
	exp := time.Now().Add(ttl)
	tok := files.Sign(s.storageSecret(sc.svc.Ref), files.Token{Kind: files.TokenGet, Ref: sc.svc.Ref, Bucket: bucket, Path: path, Exp: exp.Unix()})
	return base + "/storage/v1/object/sign/" + bucket + "/" + files.EscapePath(path) + "?token=" + tok, exp, nil
}

// spooled is an upload written to a temporary file.
type spooled struct{ f *os.File }

func (s spooled) close() { _ = s.f.Close() }

// spoolTemp writes up to limit bytes of r to a temporary file (removed on
// close) and returns it rewound, with its size.
func spoolTemp(r io.Reader, limit int64) (spooled, int64, error) {
	f, err := os.CreateTemp("", "xenibase-file-*")
	if err != nil {
		return spooled{}, 0, err
	}
	_ = os.Remove(f.Name())
	n, err := io.Copy(f, io.LimitReader(r, limit+1))
	if err != nil {
		_ = f.Close()
		return spooled{}, 0, err
	}
	if n > limit {
		_ = f.Close()
		return spooled{}, 0, fmt.Errorf("%w: the file is larger than this bucket allows (%d bytes)", ErrInvalid, limit)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return spooled{}, 0, err
	}
	return spooled{f: f}, n, nil
}
