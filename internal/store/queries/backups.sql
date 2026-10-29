-- name: InsertBackup :one
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
INSERT INTO backups (project_id, kind, object_key, started_at, status, storage_target_id, operation_id, key_wrapped, expires_at, encryption_key_id, copy_of)
VALUES (sqlc.narg(project_id), @kind, @object_key, now(), 'running', @storage_target_id, sqlc.narg(operation_id), @key_wrapped, sqlc.narg(expires_at),
        sqlc.narg(encryption_key_id), sqlc.narg(copy_of))
RETURNING *;

-- name: FinishBackup :one
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
UPDATE backups SET status = 'succeeded', size_bytes = @size_bytes, checksum = @checksum, finished_at = now()
WHERE id = @id
RETURNING *;

-- name: FailBackup :exec
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
UPDATE backups SET status = 'failed', error = @error::text, finished_at = now() WHERE id = @id;

-- name: MarkBackupDeleted :exec
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
UPDATE backups SET status = 'deleted', deleted_at = now() WHERE id = @id;

-- name: GetBackup :one
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
SELECT * FROM backups WHERE id = @id;

-- name: ListProjectBackups :many
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
SELECT * FROM backups
WHERE project_id = @project_id AND status <> 'deleted'
ORDER BY started_at DESC
LIMIT @max_rows;

-- name: LatestSucceededBackup :one
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
-- The newest logical backup (base backups restore only by PITR).
SELECT * FROM backups
WHERE project_id = @project_id AND status = 'succeeded' AND kind IN ('logical', 'final', 'safety')
ORDER BY finished_at DESC
LIMIT 1;

-- RetentionCandidates lists the succeeded backups of one kind for a project
-- (or metadata backups when project_id is null), newest first.
-- name: RetentionCandidates :many
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
SELECT * FROM backups
WHERE kind = @kind AND status = 'succeeded'
  AND ((sqlc.narg(project_id)::uuid IS NULL AND project_id IS NULL) OR project_id = sqlc.narg(project_id))
ORDER BY finished_at DESC;

-- name: ExpiredBackups :many
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
-- A copied original expires like any other backup (its copy carries the
-- same expiry and takes it along when it goes first).
SELECT * FROM backups
WHERE status IN ('succeeded', 'copied') AND expires_at IS NOT NULL AND expires_at < now();

-- name: StaleRunningBackups :many
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
SELECT * FROM backups WHERE status = 'running' AND started_at < @before;

-- name: LastBackupTimes :many
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
SELECT project_id, max(finished_at)::timestamptz AS last_backup_at
FROM backups
WHERE status = 'succeeded' AND project_id IS NOT NULL AND kind IN ('logical','base','final','safety')
GROUP BY project_id;

-- name: ProjectsDueForBackup :many
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
-- Active projects created before the window that opened at @since, with no
-- backup operation since then and no operation in flight (the next tick
-- picks them up). A project created after the window opened waits for the
-- next one. A suspended (or deleting) organisation's backups pause.
-- Branches are skipped unless a project admin turned their backups on.
SELECT p.* FROM projects p
WHERE p.deleted_at IS NULL AND p.status = 'active' AND p.created_at < @since
  AND p.lifecycle = 'active' -- a paused project hasn't changed, and refuses connections (V3 §4.2)
  AND (p.parent_project_id IS NULL OR p.branch_backups)
  AND EXISTS (SELECT 1 FROM organizations o WHERE o.id = p.org_id AND o.status = 'active')
  AND NOT EXISTS (
    SELECT 1 FROM operations o
    WHERE o.project_id = p.id AND o.kind IN ('backup', 'base_backup') AND o.created_at >= @since
  )
  AND NOT EXISTS (
    SELECT 1 FROM operations o
    WHERE o.project_id = p.id AND o.status IN ('queued', 'running')
  );

-- name: LastOperationOfKind :one
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
SELECT * FROM operations WHERE kind = @kind ORDER BY created_at DESC LIMIT 1;

-- name: RandomProjectWithBackup :one
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
SELECT p.* FROM projects p
WHERE p.deleted_at IS NULL AND p.status = 'active' AND p.lifecycle <> 'archived'
  AND EXISTS (SELECT 1 FROM backups b WHERE b.project_id = p.id AND b.status = 'succeeded' AND b.kind = 'logical')
ORDER BY random()
LIMIT 1;

-- name: RandomProjectWithBaseBackup :one
-- tenant: system - the restore test picks a dedicated project to recover, across organisations.
SELECT p.* FROM projects p JOIN instances i ON i.id = p.instance_id
WHERE p.deleted_at IS NULL AND p.status = 'active' AND p.tier = 'dedicated'
  AND i.status = 'running' AND i.walg_prefix IS NOT NULL
  AND EXISTS (SELECT 1 FROM backups b WHERE b.project_id = p.id AND b.status = 'succeeded' AND b.kind = 'base')
ORDER BY random()
LIMIT 1;

-- name: BackupForOperation :one
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
SELECT * FROM backups
WHERE operation_id = @operation_id AND kind = @kind AND status = 'succeeded'
ORDER BY finished_at DESC
LIMIT 1;

-- name: ListAllBackups :many
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
-- Backups of live and deleted projects (final backups outlive their project).
SELECT b.*, p.name AS project_name, (p.deleted_at IS NOT NULL)::bool AS project_deleted
FROM backups b
LEFT JOIN projects p ON p.id = b.project_id
WHERE b.status <> 'deleted' AND (sqlc.narg(kind)::text IS NULL OR b.kind = sqlc.narg(kind))
ORDER BY b.started_at DESC
LIMIT @max_rows;

-- name: ListRestoreTests :many
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
SELECT * FROM operations WHERE kind = 'restore_test' ORDER BY created_at DESC LIMIT @max_rows;

-- name: InsertBaseBackup :one
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
INSERT INTO backups (project_id, kind, object_key, started_at, finished_at, status, size_bytes, storage_target_id, operation_id, encryption_key_id, walg_prefix)
VALUES (@project_id, 'base', @object_key, @started_at, @finished_at, 'succeeded', @size_bytes, @storage_target_id, sqlc.narg(operation_id),
        sqlc.narg(encryption_key_id), @walg_prefix)
RETURNING *;

-- name: ListBaseBackups :many
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
-- With walg_prefix, only those under it: after a storage or key switch, the
-- earlier archive's base backups are not in the current WAL-G listing.
SELECT * FROM backups WHERE project_id = @project_id AND kind = 'base' AND status = 'succeeded'
  AND (sqlc.narg(walg_prefix)::text IS NULL OR walg_prefix = sqlc.narg(walg_prefix)) ORDER BY finished_at;

-- BaseBackupBefore is the newest base backup of a project finished at or
-- before a time (the starting point of a point-in-time recovery).
-- name: BaseBackupBefore :one
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
SELECT * FROM backups
WHERE project_id = @project_id AND kind = 'base' AND status = 'succeeded' AND finished_at <= @before
ORDER BY finished_at DESC
LIMIT 1;

-- name: FailInterruptedBackups :execrows
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
-- Backups an earlier attempt of the operation left running (its worker or
-- agent died mid-dump): they never finished.
UPDATE backups SET status = 'failed', error = 'interrupted: the attempt that took it did not finish', finished_at = now()
WHERE operation_id = @operation_id AND status = 'running';

-- name: SweepStaleBackups :execrows
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
-- Running backups whose operation has finished (or, without one, that
-- started over a day ago) were interrupted.
UPDATE backups b SET status = 'failed', error = 'interrupted: the operation ended before it finished', finished_at = now()
WHERE b.status = 'running' AND (
  EXISTS (SELECT 1 FROM operations o WHERE o.id = b.operation_id AND o.status IN ('succeeded', 'failed'))
  OR (b.operation_id IS NULL AND b.started_at < now() - interval '1 day'));

-- name: FailedBackupObjects :many
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
-- Failed backups whose object may still be in storage: an upload can
-- complete at the bucket after its attempt gave up on it.
SELECT * FROM backups
WHERE status = 'failed' AND deleted_at IS NULL AND storage_target_id IS NOT NULL
ORDER BY started_at LIMIT 100;

-- name: MarkFailedBackupCleaned :exec
-- tenant: system - backup workers and the scheduler, or a project the request already authorized.
UPDATE backups SET deleted_at = now() WHERE id = @id AND status = 'failed';

-- name: SetBackupFinished :one
-- tenant: system - copy-existing writes the copy's row as the original's twin.
UPDATE backups SET status = 'succeeded', size_bytes = @size_bytes, checksum = @checksum,
  started_at = @started_at, finished_at = @finished_at
WHERE id = @id
RETURNING *;

-- name: MarkBackupCopied :exec
-- tenant: system - copy-existing marks the original once its copy verified.
UPDATE backups SET status = 'copied' WHERE id = @id AND status = 'succeeded';

-- name: CopyCandidates :many
-- tenant: system - copy-existing for a project the request already authorized.
-- A project's logical backups that are not on the target and not yet
-- copied there. Base backups stay with their WAL-G archive.
SELECT * FROM backups b
WHERE b.project_id = @project_id AND b.status = 'succeeded' AND b.kind IN ('logical', 'final', 'safety')
  AND b.storage_target_id IS DISTINCT FROM @storage_target_id::uuid
  AND NOT EXISTS (SELECT 1 FROM backups c WHERE c.copy_of = b.id AND c.status IN ('running', 'succeeded'))
ORDER BY b.finished_at;

-- name: CopiedOriginal :one
-- tenant: system - retention removes a copy's original along with it.
SELECT * FROM backups WHERE id = @id AND status = 'copied' AND deleted_at IS NULL;

-- name: VerifiedCopiesOf :many
-- tenant: system - copy-existing checks an original's copies before deleting it.
SELECT * FROM backups WHERE copy_of = @copy_of AND status = 'succeeded' AND deleted_at IS NULL;

-- name: CopiedOriginals :many
-- tenant: system - copy-existing with delete_originals for a project the request already authorized.
SELECT * FROM backups WHERE project_id = @project_id AND status = 'copied' AND deleted_at IS NULL;

-- name: ExpireArchiveBaseBackups :exec
-- tenant: system - a storage or key switch retires a dedicated project's previous WAL-G archive.
-- Base backups under an archive no longer written to expire after the
-- point-in-time window; the archive goes with the last of them.
UPDATE backups SET expires_at = @expires_at::timestamptz
WHERE project_id = @project_id AND kind = 'base' AND status = 'succeeded' AND walg_prefix = @walg_prefix AND expires_at IS NULL;

-- name: LiveBackupsUnderPrefix :one
-- tenant: system - deciding whether a retired WAL-G archive can go.
SELECT count(*)::int FROM backups
WHERE kind = 'base' AND status = 'succeeded' AND walg_prefix = @walg_prefix AND storage_target_id = @storage_target_id;

-- name: ExpireDeletedProjectBackups :execrows
-- tenant: system - a project delete, once the project is gone.
-- Only the final backup outlives a deleted project, for 30 days (V2 s10.10):
-- its nightly and archive backups on platform targets expire with it (or
-- at @expires_at without one). Org targets are the organisation's and are
-- left alone.
UPDATE backups b SET expires_at = COALESCE(
    (SELECT max(f.expires_at) FROM backups f
     WHERE f.project_id = @project_id::uuid AND f.kind = 'final' AND f.status IN ('succeeded', 'copied')),
    @expires_at::timestamptz)
WHERE b.project_id = @project_id::uuid AND b.kind IN ('logical', 'archive') AND b.status IN ('succeeded', 'copied')
  AND b.expires_at IS NULL
  AND (b.storage_target_id IS NULL OR EXISTS (SELECT 1 FROM storage_targets t WHERE t.id = b.storage_target_id AND t.org_id IS NULL));

-- name: ExpireBackupAt :exec
-- tenant: system - an archived project's archive backup expires once the project is deleted (V3 §4.3).
UPDATE backups SET expires_at = @expires_at WHERE id = @id;

-- CopyQueue lists backups on platform targets that still need their
-- cross-region copy (V3 §2.5), with the region's copy target. Org targets
-- aren't copied; base backups are WAL-G archives, copied by the copy
-- target's own replication if at all.
-- name: CopyQueue :many
-- tenant: system - the cross-region copy worker.
SELECT b.*, r.copy_target_id AS region_copy_target_id, p.region AS project_region, p.data_residency AS project_residency,
       ct.xenibase_region AS copy_target_region
FROM backups b
JOIN projects p ON p.id = b.project_id
JOIN regions r ON r.id = p.region
JOIN storage_targets st ON st.id = b.storage_target_id
JOIN storage_targets ct ON ct.id = r.copy_target_id
WHERE b.status = 'succeeded' AND b.checksum IS NOT NULL AND b.copy_of IS NULL
  AND b.kind IN ('logical', 'final', 'safety')
  AND st.org_id IS NULL AND st.deleted_at IS NULL AND ct.deleted_at IS NULL
  AND (b.copy_status IS NULL OR b.copy_status = 'pending'
       OR (b.copy_status = 'failed' AND b.copied_at < now() - interval '1 hour'))
ORDER BY b.finished_at
LIMIT @lim;

-- name: SetBackupCopy :exec
-- tenant: system - the cross-region copy worker records a copy's outcome; copied_at is the attempt's time.
UPDATE backups SET copy_status = @copy_status, copy_target_id = sqlc.narg(copy_target_id),
  copy_error = sqlc.narg(copy_error), copied_at = now()
WHERE id = @id;

-- name: LatestCopiedBackup :one
-- tenant: system - the restore test, alternating to copy targets.
SELECT * FROM backups
WHERE project_id = @project_id AND status = 'succeeded' AND copy_status = 'copied' AND kind IN ('logical', 'final', 'safety')
ORDER BY finished_at DESC
LIMIT 1;

-- name: RandomProjectWithCopiedBackup :one
-- tenant: system - the restore test, alternating to copy targets.
SELECT p.* FROM projects p
WHERE p.deleted_at IS NULL AND p.status = 'active' AND p.lifecycle <> 'archived'
  AND EXISTS (SELECT 1 FROM backups b WHERE b.project_id = p.id AND b.status = 'succeeded' AND b.kind = 'logical' AND b.copy_status = 'copied')
ORDER BY random()
LIMIT 1;

-- name: CopyStatusCounts :many
-- tenant: system - platform admin's view of cross-region copies.
SELECT COALESCE(copy_status, 'none')::text AS copy_status, count(*)::int AS n
FROM backups WHERE status = 'succeeded' AND started_at > now() - interval '7 days'
GROUP BY 1 ORDER BY 1;

-- name: OutOfRegionCopies :many
-- tenant: system - turning data residency on removes copies outside the project's region.
SELECT b.* FROM backups b
JOIN storage_targets ct ON ct.id = b.copy_target_id
WHERE b.project_id = @project_id AND b.copy_status = 'copied'
  AND (ct.xenibase_region IS NULL OR ct.xenibase_region <> @region::text);
