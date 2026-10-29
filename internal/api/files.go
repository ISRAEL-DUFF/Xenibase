package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/israel-duff/xenibase/internal/api/gen"
	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/services"
)

// Project → Storage (V4 §5): buckets and files, managed from the dashboard
// as the platform (the project's policies are for its app's users).

func bucketOut(b services.Bucket) gen.StorageBucket {
	return gen.StorageBucket{Id: b.ID, Public: b.Public, FileSizeLimit: b.FileSizeLimit, AllowedMimeTypes: b.AllowedMIMETypes,
		CacheSeconds: b.CacheSeconds, Objects: b.Objects, Bytes: b.Bytes, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt}
}

func fileOut(o *services.FileObject) gen.StorageObject {
	return gen.StorageObject{Id: o.ID, Bucket: o.Bucket, Path: o.Path, Size: o.Size, MimeType: o.MIMEType, Etag: o.ETag,
		Checksum: o.Checksum, Owner: o.Owner, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}
}

// GetProjectFiles implements GET /api/v1/projects/{id}/files.
func (s *Server) GetProjectFiles(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	st, err := s.services.Storage(r.Context(), id)
	if err != nil {
		s.servicesError(w, "storage", err)
		return
	}
	out := gen.StorageOverview{Bytes: st.Bytes, Objects: st.Objects, QuotaBytes: st.QuotaBytes, UploadMaxBytes: st.UploadMaxBytes,
		EgressBlocked: st.EgressBlocked, TransformsBlocked: st.TransformsBlocked, MeasuredAt: st.MeasuredAt, MissingObjects: int(st.MissingObjects),
		MissingSample: st.MissingSample, ReconciledAt: st.ReconciledAt, Buckets: []gen.StorageBucket{}}
	for _, b := range st.Buckets {
		out.Buckets = append(out.Buckets, bucketOut(b))
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateStorageBucket implements POST /api/v1/projects/{id}/files/buckets.
func (s *Server) CreateStorageBucket(w http.ResponseWriter, r *http.Request, id gen.ProjectID) {
	if !s.requireServices(w) {
		return
	}
	var in gen.StorageBucketInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	auditFrom(r.Context()).target("bucket", in.Id)
	b, err := s.services.CreateBucket(r.Context(), id, in.Id, services.BucketInput{Public: in.Public, FileSizeLimit: in.FileSizeLimit,
		AllowedMIMETypes: in.AllowedMimeTypes, CacheSeconds: in.CacheSeconds})
	if err != nil {
		s.servicesError(w, "create bucket", err)
		return
	}
	writeJSON(w, http.StatusCreated, bucketOut(b))
}

// UpdateStorageBucket implements PATCH /api/v1/projects/{id}/files/buckets/{bucket}.
func (s *Server) UpdateStorageBucket(w http.ResponseWriter, r *http.Request, id gen.ProjectID, bucket gen.BucketID) {
	if !s.requireServices(w) {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	var in gen.StorageBucketUpdate
	if err != nil || json.Unmarshal(raw, &in) != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	// An explicit null removes the size limit.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	unlimit := string(fields["file_size_limit"]) == "null"
	auditFrom(r.Context()).target("bucket", bucket)
	b, err := s.services.UpdateBucket(r.Context(), id, bucket, services.BucketInput{Public: in.Public, FileSizeLimit: in.FileSizeLimit,
		Unlimit: unlimit, AllowedMIMETypes: in.AllowedMimeTypes, CacheSeconds: in.CacheSeconds})
	if err != nil {
		s.servicesError(w, "update bucket", err)
		return
	}
	writeJSON(w, http.StatusOK, bucketOut(b))
}

// DeleteStorageBucket implements DELETE /api/v1/projects/{id}/files/buckets/{bucket}.
func (s *Server) DeleteStorageBucket(w http.ResponseWriter, r *http.Request, id gen.ProjectID, bucket gen.BucketID, params gen.DeleteStorageBucketParams) {
	if !s.requireServices(w) {
		return
	}
	auditFrom(r.Context()).target("bucket", bucket)
	if err := s.services.DeleteBucket(r.Context(), id, bucket, params.Empty != nil && *params.Empty); err != nil {
		s.servicesError(w, "delete bucket", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListStorageObjects implements GET /api/v1/projects/{id}/files/buckets/{bucket}/objects.
func (s *Server) ListStorageObjects(w http.ResponseWriter, r *http.Request, id gen.ProjectID, bucket gen.BucketID, params gen.ListStorageObjectsParams) {
	if !s.requireServices(w) {
		return
	}
	prefix, cursor, limit := "", "", 100
	if params.Prefix != nil {
		prefix = *params.Prefix
	}
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	if params.Limit != nil {
		limit = *params.Limit
	}
	es, next, err := s.services.ListFiles(r.Context(), id, bucket, prefix, cursor, limit)
	if err != nil {
		s.servicesError(w, "list files", err)
		return
	}
	out := gen.StorageObjectList{Items: []gen.StorageEntry{}}
	for _, e := range es {
		en := gen.StorageEntry{Name: e.Name, Path: e.Path, Folder: e.Folder}
		if e.Object != nil {
			o := fileOut(e.Object)
			en.Object = &o
		}
		out.Items = append(out.Items, en)
	}
	if next != "" {
		out.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, out)
}

// DeleteStorageObjects implements DELETE /api/v1/projects/{id}/files/buckets/{bucket}/objects.
func (s *Server) DeleteStorageObjects(w http.ResponseWriter, r *http.Request, id gen.ProjectID, bucket gen.BucketID) {
	if !s.requireServices(w) {
		return
	}
	var in gen.DeleteStorageObjectsJSONBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	auditFrom(r.Context()).target("bucket", bucket)
	n, err := s.services.DeleteFiles(r.Context(), id, bucket, in.Paths)
	if err != nil {
		s.servicesError(w, "delete files", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": n})
}

// DownloadStorageObject implements GET /api/v1/projects/{id}/files/buckets/{bucket}/object.
func (s *Server) DownloadStorageObject(w http.ResponseWriter, r *http.Request, id gen.ProjectID, bucket gen.BucketID, params gen.DownloadStorageObjectParams) {
	if !s.requireServices(w) {
		return
	}
	o, body, err := s.services.OpenFile(r.Context(), id, bucket, params.Path)
	if err != nil {
		s.servicesError(w, "download file", err)
		return
	}
	defer body.Close()
	h := w.Header()
	h.Set("Content-Type", o.MIMEType)
	h.Set("Content-Length", strconv.FormatInt(o.Size, 10))
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(baseName(o.Path)))
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// UploadStorageObject implements PUT /api/v1/projects/{id}/files/buckets/{bucket}/object.
func (s *Server) UploadStorageObject(w http.ResponseWriter, r *http.Request, id gen.ProjectID, bucket gen.BucketID, params gen.UploadStorageObjectParams) {
	if !s.requireServices(w) {
		return
	}
	auditFrom(r.Context()).target("file", bucket+"/"+params.Path)
	o, err := s.services.UploadFile(r.Context(), id, bucket, params.Path, r.Header.Get("Content-Type"),
		http.MaxBytesReader(w, r.Body, services.DashboardUploadMax+1))
	if err != nil {
		s.servicesError(w, "upload file", err)
		return
	}
	writeJSON(w, http.StatusOK, fileOut(o))
}

// SignStorageObject implements POST /api/v1/projects/{id}/files/buckets/{bucket}/sign.
func (s *Server) SignStorageObject(w http.ResponseWriter, r *http.Request, id gen.ProjectID, bucket gen.BucketID) {
	if !s.requireServices(w) {
		return
	}
	var in gen.SignStorageObjectJSONBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	ttl := time.Hour
	if in.ExpiresIn != nil {
		ttl = time.Duration(*in.ExpiresIn) * time.Second
	}
	u, exp, err := s.services.SignFile(r.Context(), id, bucket, in.Path, ttl)
	if err != nil {
		s.servicesError(w, "sign file", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"signed_url": u, "expires_at": exp.UTC()})
}

// EdgeStorageEvent implements POST /api/v1/edge/storage-event.
func (s *Server) EdgeStorageEvent(w http.ResponseWriter, r *http.Request) {
	body, scope, ok := s.edgeAuth(w, r)
	if !ok {
		return
	}
	var ev edgeapi.StorageEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "not a storage event")
		return
	}
	if !s.edgeRef(w, r, scope, ev.Ref, "storage event") {
		return
	}
	if err := s.services.StorageEvent(r.Context(), ev); err != nil {
		s.servicesError(w, "storage event", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
