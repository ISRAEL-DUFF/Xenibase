-- name: EnqueueOperation :one
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
INSERT INTO operations (kind, project_id, params, created_by)
VALUES (@kind, sqlc.narg(project_id), @params, sqlc.narg(created_by))
RETURNING *;

-- name: GetOperation :one
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
SELECT * FROM operations WHERE id = @id;

-- name: ListOperations :many
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
SELECT * FROM operations
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind))
  AND (sqlc.narg(project_id)::uuid IS NULL OR project_id = sqlc.narg(project_id))
ORDER BY created_at DESC, id DESC
LIMIT @max_rows;

-- ClaimOperation takes the next runnable operation of one of the given
-- kinds. SKIP LOCKED lets any number of workers claim concurrently without
-- blocking each other or double-claiming.
-- name: ClaimOperation :one
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations
SET status = 'running', attempts = attempts + 1, locked_by = @worker::text, locked_at = now()
WHERE id = (
  SELECT o.id FROM operations o
  WHERE o.status = 'queued' AND o.run_after <= now() AND o.kind = ANY(@kinds::text[])
  ORDER BY o.run_after, o.created_at
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING *;

-- The writes below are fenced by worker and attempt: an attempt whose lease
-- was reclaimed (even one re-claimed by the same worker) writes nothing.
-- name: HeartbeatOperation :execrows
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations SET locked_at = now()
WHERE id = @id AND locked_by = @worker::text AND attempts = @attempt::int AND status = 'running';

-- name: AppendOperationLog :execrows
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations SET log = log || jsonb_build_array(@entry::jsonb)
WHERE id = @id AND locked_by = @worker::text AND attempts = @attempt::int AND status = 'running';

-- params.secrets carries encrypted, short-lived handoff data (for example
-- a new password for the smoke test). It is scrubbed when an operation
-- finishes either way.
-- name: SucceedOperation :execrows
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations
SET status = 'succeeded', error = NULL, params = params - 'secrets', finished_at = now(), locked_by = NULL, locked_at = NULL
WHERE id = @id AND locked_by = @worker::text AND attempts = @attempt::int AND status = 'running';

-- name: FailOperation :execrows
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations
SET status = 'failed', error = @error::text, params = params - 'secrets', finished_at = now(), locked_by = NULL, locked_at = NULL
WHERE id = @id AND locked_by = @worker::text AND attempts = @attempt::int AND status = 'running';

-- name: RetryOperation :execrows
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations
SET status = 'queued', error = @error::text, run_after = @run_after, locked_by = NULL, locked_at = NULL
WHERE id = @id AND locked_by = @worker::text AND attempts = @attempt::int AND status = 'running';

-- ReclaimStaleOperations requeues running operations whose worker stopped
-- heartbeating (crashed or partitioned), so they resume elsewhere.
-- name: ReclaimStaleOperations :many
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations
SET status = 'queued', locked_by = NULL, locked_at = NULL,
    log = log || jsonb_build_array(jsonb_build_object(
      'ts', now(), 'step', 'queue', 'level', 'warn',
      'msg', 'worker ' || locked_by || ' stopped heartbeating; requeued'))
WHERE status = 'running' AND locked_at < @stale_before::timestamptz
RETURNING id;

-- name: ProjectHasActiveOperation :one
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
SELECT EXISTS (
  SELECT 1 FROM operations
  WHERE project_id = @project_id AND status IN ('queued', 'running')
);
