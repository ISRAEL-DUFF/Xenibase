package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/israel-duff/xenibase/internal/edgeapi"
	"github.com/israel-duff/xenibase/internal/files"
	"github.com/israel-duff/xenibase/internal/storage"
	"github.com/israel-duff/xenibase/internal/store"
	"github.com/israel-duff/xenibase/internal/tenancy"
)

// The control plane's side of storage (V4 §5.6, §10): an hourly-or-so sweep
// measures each project's files (correcting the usage the edge checks
// quotas against), meters storage_gb_hours, removes bytes no row points at
// any more and abandoned uploads, works out the quotas the edge enforces,
// and removes deleted projects' files after the final backups' retention;
// a nightly reconciler removes orphaned bytes and finds rows whose bytes
// are missing.

const (
	// StorageSweepEvery is how often the sweep runs.
	StorageSweepEvery = 5 * time.Minute
	// ReconcileStorageEvery is how often orphans and missing bytes are
	// looked for.
	ReconcileStorageEvery = 24 * time.Hour
	// garbageGrace leaves replaced and deleted bytes for downloads in
	// flight.
	garbageGrace = time.Minute
	// orphanAge is how old bytes without a row must be to go (a row may be
	// about to be written).
	orphanAge = 7 * 24 * time.Hour
	// deletedFilesDays keeps a deleted project's files as long as its final
	// backup (V2 §10.10).
	deletedFilesDays = 30
)

// filesClient is the object store for files of projects in region.
func (s *Service) filesClient(ctx context.Context, region string, residency bool) (*storage.Client, error) {
	if s.Files == nil {
		return nil, errors.New("file storage is not configured")
	}
	t, err := s.Files(ctx, region, residency)
	if err != nil {
		return nil, err
	}
	return storage.New(t)
}

func objectsKey(ref string, v uuid.UUID) string { return files.ObjectKey(files.Prefix(ref), v) }

// StorageSweep runs the sweep once.
func (s *Service) StorageSweep(ctx context.Context) error {
	if s.Files == nil {
		return nil
	}
	q := store.New(s.db)
	ps, err := q.StorageProjects(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range ps {
		if err := s.sweepProject(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Ref, err))
		}
	}
	if err := s.meterStorage(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.storageLimits(ctx, ps); err != nil {
		errs = append(errs, err)
	}
	if err := s.abandonedUploads(ctx, ps); err != nil {
		errs = append(errs, err)
	}
	if err := s.cleanupDeleted(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.RetryCDNPurges(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// sweepProject measures one project's files, meters them for the hour,
// and removes what it no longer needs.
func (s *Service) sweepProject(ctx context.Context, r store.StorageProjectsRow) error {
	p := r.Project
	conn, err := s.projects.AdminConn(ctx, p.InstanceID, p.DbName)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var bytes, objects int64
	if err := conn.QueryRow(ctx, `SELECT coalesce(sum(size), 0)::bigint, count(*) FROM xb_storage.objects`).Scan(&bytes, &objects); err != nil {
		return err
	}
	// The trigger-kept usage drifts if the owner bypasses it (TRUNCATE).
	if _, err := conn.Exec(ctx, `UPDATE xb_storage.usage SET bytes = $1, objects = $2 WHERE bytes <> $1 OR objects <> $2`, bytes, objects); err != nil {
		return err
	}
	if err := store.New(s.db).UpsertProjectStorage(ctx, store.UpsertProjectStorageParams{ProjectID: p.ID, Bytes: bytes, Objects: objects}); err != nil {
		return err
	}
	cl, err := s.filesClient(ctx, p.Region, p.DataResidency)
	if err != nil {
		return err
	}
	grace := s.StorageGrace
	if grace == 0 {
		grace = garbageGrace
	}
	if _, err := collectGarbage(ctx, conn, cl, r.Ref, grace); err != nil {
		return fmt.Errorf("clean-up: %w", err)
	}
	// Uploads not completed in time: abandoned.
	rows, err := conn.Query(ctx, `DELETE FROM xb_storage.uploads WHERE expires_at < now() RETURNING version, upload_id`)
	if err != nil {
		return err
	}
	type expired struct {
		version  uuid.UUID
		uploadID string
	}
	var gone []expired
	for rows.Next() {
		var x expired
		if err := rows.Scan(&x.version, &x.uploadID); err != nil {
			rows.Close()
			return err
		}
		gone = append(gone, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, x := range gone {
		if err := cl.AbortMultipart(ctx, objectsKey(r.Ref, x.version), x.uploadID); err != nil {
			return err
		}
	}
	// Signed upload URLs never used (schema version 8 on).
	_, err = conn.Exec(ctx, `DO $$ BEGIN
		IF to_regclass('xb_storage.upload_tokens') IS NOT NULL THEN DELETE FROM xb_storage.upload_tokens WHERE expires_at < now(); END IF;
	END $$`)
	return err
}

// meterStorage records this hour's storage_gb_hours for every live
// project holding files, from its last measurement: the files and their
// cached renders, whether or not its backend services are on (turning them
// off keeps the files) and whether or not this sweep reached it. A replace,
// so several servers meter an hour once.
func (s *Service) meterStorage(ctx context.Context) error {
	q := store.New(s.db)
	rows, err := q.StorageToMeter(ctx)
	if err != nil {
		return err
	}
	hour := time.Now().UTC().Truncate(time.Hour)
	var errs []error
	for _, r := range rows {
		if err := q.UpsertUsage(ctx, store.UpsertUsageParams{OrgID: r.OrgID, ProjectID: r.ProjectID, Metric: tenancy.MetricStorageGBHours,
			Granularity: "hour", PeriodStart: hour, Quantity: gbNumeric(r.Bytes + r.TransformBytes), PlanID: r.PlanID}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// collectGarbage removes the bytes (and renders) of versions no row points
// at, queued longer than grace.
func collectGarbage(ctx context.Context, conn *pgx.Conn, cl *storage.Client, ref string, grace time.Duration) (int, error) {
	total := 0
	for {
		n := 0
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT g.version, EXISTS (SELECT 1 FROM xb_storage.objects o WHERE o.version = g.version)
				FROM xb_storage.garbage g WHERE g.at < now() - make_interval(secs => $1) ORDER BY g.at LIMIT 200 FOR UPDATE SKIP LOCKED`, grace.Seconds())
			if err != nil {
				return err
			}
			var done []uuid.UUID
			var drop []uuid.UUID
			for rows.Next() {
				var v uuid.UUID
				var used bool
				if err := rows.Scan(&v, &used); err != nil {
					rows.Close()
					return err
				}
				done = append(done, v)
				if !used {
					drop = append(drop, v)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, v := range drop {
				if err := cl.Delete(ctx, objectsKey(ref, v)); err != nil {
					return err
				}
				if _, err := cl.DeletePrefix(ctx, files.TransformsPrefix(files.Prefix(ref), v)); err != nil {
					return err
				}
			}
			n = len(done)
			_, err = tx.Exec(ctx, `DELETE FROM xb_storage.garbage WHERE version = ANY($1)`, done)
			return err
		})
		total += n
		if err != nil || n < 200 {
			return total, err
		}
	}
}

// storageLimits works out what the edge enforces for each project: its
// share of the organisation's file quota, the largest upload, and whether
// the month's download egress or image transforms are used up.
func (s *Service) storageLimits(ctx context.Context, ps []store.StorageProjectsRow) error {
	q := store.New(s.db)
	byOrg := map[uuid.UUID][]store.StorageProjectsRow{}
	for _, r := range ps {
		byOrg[r.Project.OrgID] = append(byOrg[r.Project.OrgID], r)
	}
	month := time.Now().UTC()
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	var errs []error
	for org, rows := range byOrg {
		o, err := q.OrgWithPlan(ctx, org)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		l, err := store.EffectiveLimits(o.PlanLimits, o.LimitOverrides)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		files, err := q.OrgFileBytes(ctx, org)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var orgBytes int64
		per := map[uuid.UUID]int64{}
		for _, f := range files {
			per[f.ProjectID] = f.Bytes
			orgBytes += f.Bytes
		}
		egressBlocked, transformsBlocked := false, false
		if capMB, ok := l.Get(store.LimitStorageEgressMBMo); ok {
			used, err := q.OrgUsageSince(ctx, store.OrgUsageSinceParams{OrgID: org, Metric: tenancy.MetricStorageEgress, Since: month})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			egressBlocked = numericFloat(used)*1000 >= float64(capMB)
		}
		if capN, ok := l.Get(store.LimitImageTransformsMo); ok {
			used, err := q.OrgUsageSince(ctx, store.OrgUsageSinceParams{OrgID: org, Metric: tenancy.MetricImageTransforms, Since: month})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			transformsBlocked = numericFloat(used) >= float64(capN)
		}
		var uploadMax *int64
		if mb, ok := l.Get(store.LimitUploadMaxMB); ok {
			v := mb << 20
			uploadMax = &v
		}
		for _, r := range rows {
			var quota *int64
			if mb, ok := l.Get(store.LimitFileStorageMB); ok {
				// What the others hold, rounded to a megabyte so small
				// changes don't move the feed.
				others := (orgBytes - per[r.Project.ID]) >> 20 << 20
				v := max((mb<<20)-others, 1)
				quota = &v
			}
			if _, err := q.SetStorageLimits(ctx, store.SetStorageLimitsParams{ProjectID: r.Project.ID, QuotaBytes: quota, UploadMaxBytes: uploadMax,
				EgressBlocked: egressBlocked, TransformsBlocked: transformsBlocked}); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func numericFloat(n pgtype.Numeric) float64 {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 0
	}
	return f.Float64
}

// abandonedUploads aborts multipart uploads older than a day in each
// region's store (an edge that started one and lost its row, or a client
// that never finished).
func (s *Service) abandonedUploads(ctx context.Context, ps []store.StorageProjectsRow) error {
	seen := map[[2]any]bool{}
	var errs []error
	for _, r := range ps {
		k := [2]any{r.Project.Region, r.Project.DataResidency}
		if seen[k] {
			continue
		}
		seen[k] = true
		cl, err := s.filesClient(ctx, r.Project.Region, r.Project.DataResidency)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ups, err := cl.Multiparts(ctx, "files/")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, u := range ups {
			if time.Since(u.Initiated) > 25*time.Hour {
				if err := cl.AbortMultipart(ctx, u.Key, u.ID); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// cleanupDeleted schedules deleted projects' files to go and removes those
// due.
func (s *Service) cleanupDeleted(ctx context.Context) error {
	q := store.New(s.db)
	if _, err := q.ScheduleStorageCleanups(ctx, deletedFilesDays); err != nil {
		return err
	}
	due, err := q.DueStorageCleanups(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range due {
		cl, err := s.filesClient(ctx, c.Region, c.Residency)
		if err == nil {
			_, err = cl.DeletePrefix(ctx, c.Prefix)
		}
		if err != nil {
			msg := err.Error()
			errs = append(errs, err)
			if ferr := q.FailStorageCleanup(ctx, store.FailStorageCleanupParams{ID: c.ID, LastError: &msg}); ferr != nil {
				errs = append(errs, ferr)
			}
			continue
		}
		if err := q.FinishStorageCleanup(ctx, c.ID); err != nil {
			errs = append(errs, err)
		}
		s.log.Info("deleted project's files removed", "project_id", c.ProjectID, "prefix", c.Prefix)
	}
	return errors.Join(errs...)
}

// ReconcileStorageDue runs the reconciler when its last run, on any
// server, is ReconcileStorageEvery old: the time is kept in the metadata
// database, so restarts and deploys don't put it off. It reports whether
// it ran.
func (s *Service) ReconcileStorageDue(ctx context.Context) (bool, error) {
	if s.Files == nil {
		return false, nil
	}
	n, err := store.New(s.db).ClaimStorageReconcile(ctx, ReconcileStorageEvery.Seconds())
	if err != nil || n == 0 {
		return false, err
	}
	return true, s.ReconcileStorage(ctx)
}

// ReconcileStorage looks for bytes no row, upload or queued clean-up
// points at (removed once a week old) and rows whose bytes are missing
// (reported on the project's storage page), and measures what each
// project's cached renders take.
func (s *Service) ReconcileStorage(ctx context.Context) error {
	if s.Files == nil {
		return nil
	}
	ps, err := store.New(s.db).StorageProjects(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range ps {
		if err := s.reconcileProject(ctx, r, orphanAge); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Ref, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) reconcileProject(ctx context.Context, r store.StorageProjectsRow, minAge time.Duration) error {
	p := r.Project
	cl, err := s.filesClient(ctx, p.Region, p.DataResidency)
	if err != nil {
		return err
	}
	return s.reconcileFiles(ctx, cl, p.ID, r.Ref, func() (*pgx.Conn, error) { return s.projects.AdminConn(ctx, p.InstanceID, p.DbName) }, minAge)
}

// reconcileFiles reconciles a project's files in cl with the rows in the
// database connect reaches.
func (s *Service) reconcileFiles(ctx context.Context, cl *storage.Client, projectID uuid.UUID, ref string, connect func() (*pgx.Conn, error), minAge time.Duration) error {
	// The bytes there are, listed before the rows are read so an upload
	// finishing in between is never taken for an orphan.
	stored := map[string]storage.Entry{}
	rendered := map[string][]storage.Entry{} // version -> its renders
	prefix := FilesPrefix(ref)
	if err := cl.List(ctx, prefix, func(es []storage.Entry) error {
		for _, e := range es {
			rest := strings.TrimPrefix(e.Key, prefix)
			switch {
			case strings.HasPrefix(rest, "objects/"):
				stored[strings.TrimPrefix(rest, "objects/")] = e
			case strings.HasPrefix(rest, "transforms/"):
				v, _, _ := strings.Cut(strings.TrimPrefix(rest, "transforms/"), "/")
				rendered[v] = append(rendered[v], e)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	conn, err := connect()
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	known := map[string]bool{}
	var missing int32
	var sample []string
	rows, err := conn.Query(ctx, `SELECT version::text, bucket || '/' || path FROM xb_storage.objects`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v, name string
		if err := rows.Scan(&v, &name); err != nil {
			rows.Close()
			return err
		}
		known[v] = true
		if _, ok := stored[v]; !ok {
			missing++
			if len(sample) < 20 {
				sample = append(sample, name)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, sql := range []string{`SELECT version::text FROM xb_storage.uploads`, `SELECT version::text FROM xb_storage.garbage`} {
		rows, err := conn.Query(ctx, sql)
		if err != nil {
			return err
		}
		vs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, v := range vs {
			known[v] = true
		}
	}
	var removed int64
	for v, e := range stored {
		if known[v] || time.Since(e.Modified) < minAge {
			continue
		}
		if err := cl.Delete(ctx, e.Key); err != nil {
			return err
		}
		removed++
	}
	// What the kept renders take is metered with the files.
	var renders int64
	for v, es := range rendered {
		if known[v] {
			for _, e := range es {
				renders += e.Size
			}
			continue
		}
		for _, e := range es {
			if err := cl.Delete(ctx, e.Key); err != nil {
				return err
			}
		}
	}
	if missing > 0 {
		s.log.Warn("storage objects without their bytes", "project_id", projectID, "count", missing)
	}
	return store.New(s.db).SetStorageReconciled(ctx, store.SetStorageReconciledParams{ProjectID: projectID, MissingObjects: missing,
		MissingSample: orEmpty(sample), OrphansRemoved: removed, TransformBytes: renders})
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ReconcileStorageNow reconciles one project with orphans of any age
// (tests).
func (s *Service) ReconcileStorageNow(ctx context.Context, projectID uuid.UUID) error {
	ps, err := store.New(s.db).StorageProjects(ctx)
	if err != nil {
		return err
	}
	for _, r := range ps {
		if r.Project.ID == projectID {
			return s.reconcileProject(ctx, r, 0)
		}
	}
	return ErrNotFound
}

// StorageEvent acts on an edge's storage change: what left a public
// bucket's URLs (the bucket made private, deleted or emptied, objects
// deleted, replaced or moved away) leaves the CDN's cache (V4 §5.4).
func (s *Service) StorageEvent(ctx context.Context, ev edgeapi.StorageEvent) error {
	switch ev.Event {
	case edgeapi.EventBucketPrivate, edgeapi.EventBucketDeleted, edgeapi.EventBucketEmptied, edgeapi.EventObjectsChanged:
	default:
		return fmt.Errorf("%w: unknown storage event %q", ErrInvalid, ev.Event)
	}
	if !files.BucketRe.MatchString(ev.Bucket) {
		return fmt.Errorf("%w: not a bucket: %q", ErrInvalid, ev.Bucket)
	}
	svc, err := store.New(s.db).ProjectServicesByRef(ctx, ev.Ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	p, err := store.New(s.db).GetProject(ctx, svc.ProjectID)
	if err != nil {
		return err
	}
	if ev.Event != edgeapi.EventObjectsChanged {
		return s.PurgeBucket(ctx, p.ID, ev.Ref, p.Region, ev.Bucket)
	}
	for _, path := range ev.Paths {
		if !files.ValidPath(path) {
			return fmt.Errorf("%w: not an object path: %q", ErrInvalid, path)
		}
	}
	return s.PurgeObjects(ctx, p.ID, ev.Ref, p.Region, ev.Bucket, ev.Paths)
}

// publicRoutes are the keyless routes a public bucket's files and renders
// are served (and cached) at.
var publicRoutes = []string{"public", "object/public", "render/public", "render/image/public", "render"}

// purgeEachUpTo is the most objects purged one by one; more purge the
// whole bucket (Cloudflare takes 30 prefixes a request).
const purgeEachUpTo = 20

// PurgeBucket removes a bucket's public files from the CDN's cache.
func (s *Service) PurgeBucket(ctx context.Context, projectID uuid.UUID, ref, region, bucket string) error {
	base := strings.TrimPrefix(s.URL(ref, region), "https://")
	if s.CDN == nil || base == "" {
		return nil
	}
	var prefixes []string
	for _, p := range publicRoutes {
		prefixes = append(prefixes, base+"/storage/v1/"+p+"/"+bucket+"/")
	}
	return s.purge(ctx, projectID, prefixes)
}

// PurgeObjects removes public files' URLs, and their renders', from the
// CDN's cache: an object's path is a prefix of every URL it is served at.
func (s *Service) PurgeObjects(ctx context.Context, projectID uuid.UUID, ref, region, bucket string, paths []string) error {
	if len(paths) > purgeEachUpTo {
		return s.PurgeBucket(ctx, projectID, ref, region, bucket)
	}
	base := strings.TrimPrefix(s.URL(ref, region), "https://")
	if s.CDN == nil || base == "" || len(paths) == 0 {
		return nil
	}
	var prefixes []string
	for _, path := range paths {
		for _, p := range publicRoutes {
			prefixes = append(prefixes, base+"/storage/v1/"+p+"/"+bucket+"/"+files.EscapePath(path))
		}
	}
	return s.purge(ctx, projectID, prefixes)
}

// purge queues prefixes for the CDN and tries them at once; one the CDN
// doesn't take now stays queued for the sweep (RetryCDNPurges).
func (s *Service) purge(ctx context.Context, projectID uuid.UUID, prefixes []string) error {
	q := store.New(s.db)
	id, err := q.EnqueueCDNPurge(ctx, store.EnqueueCDNPurgeParams{ProjectID: projectID, Prefixes: prefixes})
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.CDN.PurgePrefixes(pctx, prefixes); err != nil {
		s.log.Warn("CDN purge failed; the storage sweep retries it", "project_id", projectID, "err", err)
		msg := err.Error()
		return q.FailCDNPurge(context.WithoutCancel(ctx), store.FailCDNPurgeParams{ID: id, LastError: &msg})
	}
	return q.FinishCDNPurge(context.WithoutCancel(ctx), id)
}

// RetryCDNPurges tries the purges the CDN hasn't taken yet, each again
// after a backoff of up to an hour, until it does.
func (s *Service) RetryCDNPurges(ctx context.Context) error {
	q := store.New(s.db)
	if _, err := q.DropStaleCDNPurges(ctx); err != nil {
		return err
	}
	if s.CDN == nil {
		return nil
	}
	due, err := q.ClaimCDNPurges(ctx, 50)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range due {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s.CDN.PurgePrefixes(pctx, p.Prefixes)
		cancel()
		if err != nil {
			msg := err.Error()
			errs = append(errs, fmt.Errorf("CDN purge (attempt %d): %w", p.Attempts+2, err))
			if ferr := q.FailCDNPurge(ctx, store.FailCDNPurgeParams{ID: p.ID, LastError: &msg}); ferr != nil {
				errs = append(errs, ferr)
			}
			continue
		}
		if err := q.FinishCDNPurge(ctx, p.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CDNPurger removes cached URLs from the CDN in front of the edges.
type CDNPurger interface {
	PurgePrefixes(ctx context.Context, prefixes []string) error
}
