-- Storage targets (V2 s6): platform targets have no org_id; org targets
-- belong to one organisation and are invisible to every other one and to
-- the platform admin.

-- name: GetDefaultStorageTarget :one
SELECT * FROM storage_targets WHERE is_default AND org_id IS NULL AND deleted_at IS NULL;

-- name: GetStorageTarget :one
-- tenant: system - backup workers resolve a project's or a backup's target by id (an org_id-free lookup the caller already scoped).
SELECT * FROM storage_targets WHERE id = @id;

-- name: GetOrgStorageTarget :one
SELECT * FROM storage_targets WHERE id = @id AND org_id = @org_id::uuid AND deleted_at IS NULL;

-- name: GetPlatformStorageTarget :one
SELECT * FROM storage_targets WHERE id = @id AND org_id IS NULL AND deleted_at IS NULL;

-- name: ListPlatformStorageTargets :many
SELECT * FROM storage_targets WHERE org_id IS NULL AND deleted_at IS NULL ORDER BY is_default DESC, name;

-- name: ListOrgStorageTargets :many
SELECT * FROM storage_targets WHERE org_id = @org_id::uuid AND deleted_at IS NULL ORDER BY name;

-- name: InsertStorageTarget :one
INSERT INTO storage_targets (id, org_id, name, endpoint, bucket, prefix, region, path_style, credentials, is_default, created_by)
VALUES (@id, sqlc.narg(org_id), @name, @endpoint, @bucket, @prefix, @region, @path_style, @credentials, @is_default, sqlc.narg(created_by))
RETURNING *;

-- name: UpdateStorageTarget :one
-- tenant: system - the caller loaded the target through its org_id (or as a platform target) first.
UPDATE storage_targets
SET name = @name, endpoint = @endpoint, bucket = @bucket, prefix = @prefix, region = @region,
    path_style = @path_style, credentials = @credentials, updated_at = now()
WHERE id = @id AND deleted_at IS NULL
RETURNING *;

-- name: ClearDefaultStorageTarget :exec
UPDATE storage_targets SET is_default = false WHERE is_default AND org_id IS NULL;

-- name: SetDefaultStorageTarget :execrows
UPDATE storage_targets SET is_default = true WHERE id = @id AND org_id IS NULL AND deleted_at IS NULL;

-- name: SoftDeleteStorageTarget :exec
-- tenant: system - the caller loaded the target through its org_id (or as a platform target) first.
UPDATE storage_targets SET deleted_at = now(), is_default = false, credentials = '\x'::bytea WHERE id = @id;

-- name: StorageTargetUse :one
-- tenant: system - deletion checks for a target the caller already scoped; counts only.
-- Live projects choosing the target (or, for the default, choosing none),
-- live dedicated instances archiving to it, and unexpired backups on it.
SELECT
  (SELECT count(*) FROM projects p WHERE p.deleted_at IS NULL
     AND (p.storage_target_id = @id::uuid OR (@is_default::bool AND p.storage_target_id IS NULL)))::int AS projects,
  (SELECT count(*) FROM instances i WHERE i.status <> 'deleted' AND i.walg_target_id = @id::uuid)::int AS instances,
  (SELECT count(*) FROM backups b WHERE b.storage_target_id = @id::uuid AND b.status IN ('succeeded', 'copied', 'running')
     AND b.deleted_at IS NULL AND (b.expires_at IS NULL OR b.expires_at > now()))::int AS backups,
  (SELECT COALESCE(sum(b.size_bytes), 0) FROM backups b WHERE b.storage_target_id = @id::uuid
     AND b.status IN ('succeeded', 'copied') AND b.deleted_at IS NULL)::bigint AS bytes;

-- name: ForgetTargetBackups :execrows
-- tenant: system - deleting a target whose backups the deleter accepted losing.
UPDATE backups SET status = 'deleted', deleted_at = now(), error = 'its storage target was deleted'
WHERE storage_target_id = @id::uuid AND status IN ('succeeded', 'copied', 'failed') AND deleted_at IS NULL;

-- name: SetProjectStorageTarget :exec
-- tenant: system - a project the request already authorized.
UPDATE projects SET storage_target_id = sqlc.narg(storage_target_id) WHERE id = @id;

-- name: ProjectsUsingOrgTargets :one
-- A project that uses one of the org's targets, or holds backups on one,
-- cannot leave the org with them.
SELECT (p.storage_target_id IS NOT NULL AND EXISTS (SELECT 1 FROM storage_targets t WHERE t.id = p.storage_target_id AND t.org_id IS NOT NULL)
        OR EXISTS (SELECT 1 FROM backups b JOIN storage_targets t ON t.id = b.storage_target_id
                   WHERE b.project_id = p.id AND t.org_id IS NOT NULL AND b.status IN ('succeeded', 'copied', 'running') AND b.deleted_at IS NULL))::bool
FROM projects p WHERE p.id = @id AND p.org_id = @org_id;

-- name: OrgPlatformBackupBytes :one
-- Backup storage the org's projects hold on platform targets (V2 s10.3);
-- org targets do not count.
SELECT COALESCE(sum(b.size_bytes), 0)::float8
FROM backups b JOIN projects p ON p.id = b.project_id JOIN storage_targets t ON t.id = b.storage_target_id
WHERE p.org_id = @org_id AND t.org_id IS NULL AND b.status IN ('succeeded', 'copied') AND b.deleted_at IS NULL;

-- name: SetInstanceWALG :exec
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
UPDATE instances SET walg_prefix = @walg_prefix, walg_target_id = @walg_target_id, walg_key_id = sqlc.narg(walg_key_id)
WHERE id = @id;

-- Per-project backup keys.

-- name: InsertBackupKey :one
-- tenant: system - a project the request already authorized.
INSERT INTO backup_keys (id, project_id, key_enc, fingerprint, created_by)
VALUES (@id, @project_id, @key_enc, @fingerprint, sqlc.narg(created_by))
RETURNING *;

-- name: GetBackupKey :one
-- tenant: system - backup workers open the key a backup row or project names.
SELECT * FROM backup_keys WHERE id = @id;

-- name: SetProjectBackupKey :exec
-- tenant: system - a project the request already authorized.
UPDATE projects SET backup_key_id = @backup_key_id WHERE id = @id;

-- name: RetireBackupKey :exec
-- tenant: system - a project the request already authorized.
UPDATE backup_keys SET retired_at = now() WHERE id = @id AND retired_at IS NULL;

-- name: GetProjectForUpdate :one
-- tenant: system - a project the request already authorized, locked while its backup settings change.
SELECT * FROM projects WHERE id = @id FOR UPDATE;

-- name: InstanceArchivingTo :one
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
SELECT EXISTS (SELECT 1 FROM instances WHERE walg_prefix = @walg_prefix AND walg_target_id = @walg_target_id AND status <> 'deleted')::bool;

-- name: DedicatedArchiveDrift :many
-- tenant: system - the backup scheduler across organisations.
-- Running dedicated projects whose WAL-G archive is not on the target and
-- key their backups now use, with nothing in flight.
SELECT p.* FROM projects p JOIN instances i ON i.id = p.instance_id
WHERE p.deleted_at IS NULL AND p.status = 'active' AND p.tier = 'dedicated'
  AND i.status = 'running' AND i.walg_prefix IS NOT NULL AND i.walg_target_id IS NOT NULL
  AND (COALESCE(p.storage_target_id, (SELECT t.id FROM storage_targets t WHERE t.is_default AND t.org_id IS NULL AND t.deleted_at IS NULL)) IS DISTINCT FROM i.walg_target_id
       OR p.backup_key_id IS DISTINCT FROM i.walg_key_id)
  AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.project_id = p.id AND o.status IN ('queued', 'running'));
