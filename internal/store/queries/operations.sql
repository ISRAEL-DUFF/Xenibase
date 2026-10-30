-- name: EnqueueOperation :one
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
-- A project has one exclusive operation queued or running at a time
-- (operations_one_exclusive_per_project); a background one may run beside it.
INSERT INTO operations (kind, project_id, params, created_by, exclusive)
VALUES (@kind, sqlc.narg(project_id), @params, sqlc.narg(created_by), @exclusive)
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
-- kinds: one of first_kinds (the interactive ones) if any is runnable,
-- otherwise the oldest. The runner leaves out of kinds the classes whose
-- slots are full. SKIP LOCKED lets any number of workers claim
-- concurrently without blocking each other or double-claiming.
-- name: ClaimOperation :one
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations
SET status = 'running', attempts = attempts + 1, locked_by = @worker::text, locked_at = now()
WHERE id = (
  SELECT o.id FROM operations o
  WHERE o.status = 'queued' AND o.run_after <= now() AND o.kind = ANY(@kinds::text[])
  ORDER BY (o.kind = ANY(@first_kinds::text[])) IS TRUE DESC, o.run_after, o.created_at
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

-- RequeueInterruptedOperation hands back an attempt that a clean shutdown
-- cut short, and gives the attempt back: only crashes count toward the
-- limit.
-- name: RequeueInterruptedOperation :execrows
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations
SET status = 'queued', attempts = GREATEST(attempts - 1, 0), error = @error::text, run_after = now(), locked_by = NULL, locked_at = NULL
WHERE id = @id AND locked_by = @worker::text AND attempts = @attempt::int AND status = 'running';

-- ReclaimStaleOperations requeues running operations whose worker stopped
-- heartbeating (crashed or partitioned), so they resume elsewhere. The
-- cutoff is on the database's clock, the one locked_at was written with.
-- name: ReclaimStaleOperations :many
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations
SET status = 'queued', locked_by = NULL, locked_at = NULL,
    log = log || jsonb_build_array(jsonb_build_object(
      'ts', now(), 'step', 'queue', 'level', 'warn',
      'msg', 'worker ' || locked_by || ' stopped heartbeating; requeued'))
WHERE status = 'running' AND locked_at < now() - make_interval(secs => @lease_seconds::float8)
RETURNING id;

-- ProjectHasActiveOperation reports any operation queued or running on
-- the project, background ones included: a new exclusive one waits for
-- them too.
-- QueuedOperationAges is, per kind, how many runnable operations wait and
-- how long the oldest has (since its run_after, on the database's clock),
-- for the queue-age alert.
-- name: QueuedOperationAges :many
-- tenant: system - the queue-age alert looks at the whole queue, every organisation's operations.
SELECT kind, count(*)::bigint AS queued,
       extract(epoch FROM now() - min(run_after))::float8 AS waited_seconds
FROM operations
WHERE status = 'queued' AND run_after <= now()
GROUP BY kind
ORDER BY kind;

-- name: ProjectHasActiveOperation :one
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
SELECT EXISTS (
  SELECT 1 FROM operations
  WHERE project_id = @project_id AND status IN ('queued', 'running')
);

-- FailUnservedOperations fails operations of a kind this worker doesn't
-- run that have waited, runnable, for longer than @after_seconds: no
-- worker runs that kind (an older or newer server's, or a typo), and a
-- pending exclusive one would block its project for good.
-- name: FailUnservedOperations :many
-- tenant: system - the job runner, or an operation the request already authorized; API lists use ListOrgOperations.
UPDATE operations
SET status = 'failed', params = params - 'secrets', finished_at = now(),
    error = 'no worker runs operations of kind ' || kind || '; gave up after waiting ' || (now() - run_after)::text,
    log = log || jsonb_build_array(jsonb_build_object(
      'ts', now(), 'step', 'queue', 'level', 'error',
      'msg', 'no worker runs operations of kind ' || kind || '; failed after waiting since ' || run_after::text))
WHERE status = 'queued' AND NOT (kind = ANY(@kinds::text[]))
  AND run_after < now() - make_interval(secs => @after_seconds::float8)
RETURNING id, kind;
