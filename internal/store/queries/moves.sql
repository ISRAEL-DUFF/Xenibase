-- name: StartMove :one
-- tenant: system - the move worker records its own progress.
INSERT INTO moves (operation_id, project_id, source_instance, target_instance, mode, fallback_reason)
VALUES (@operation_id, @project_id, @source_instance, @target_instance, @mode, sqlc.narg(fallback_reason))
ON CONFLICT (operation_id) DO UPDATE SET mode = EXCLUDED.mode, fallback_reason = EXCLUDED.fallback_reason,
  phase = 'preparing', started_at = now(), finished_at = NULL, freeze_ms = NULL
RETURNING *;

-- name: MovePhase :exec
-- tenant: system - the move worker records its own progress.
UPDATE moves SET phase = @phase, tables_total = coalesce(sqlc.narg(tables_total), tables_total),
  tables_ready = coalesce(sqlc.narg(tables_ready), tables_ready), lag_bytes = coalesce(sqlc.narg(lag_bytes), lag_bytes),
  freeze_ms = coalesce(sqlc.narg(freeze_ms), freeze_ms),
  finished_at = CASE WHEN @phase IN ('done', 'failed') THEN now() END
WHERE operation_id = @operation_id;

-- name: LatestMoves :many
-- tenant: system - moves of a project the request already authorized.
SELECT * FROM moves WHERE project_id = @project_id ORDER BY started_at DESC LIMIT @lim;

-- name: MoveCutOver :exec
-- tenant: system - move workers switch the project's instance.
-- A move to another region's node moves the project's region; the old
-- region's poolers keep routing it for 30 days (V3 §6.1).
UPDATE projects p SET instance_id = @instance_id,
  forward_region = CASE WHEN p.region <> n.region THEN p.region ELSE p.forward_region END,
  forward_until = CASE WHEN p.region <> n.region THEN now() + interval '30 days' ELSE p.forward_until END,
  region = n.region
FROM instances i JOIN nodes n ON n.id = i.node_id
WHERE i.id = @instance_id AND p.id = @id;

-- ListStuckProjects finds projects a move, promotion, demotion or major
-- upgrade left in its status with no operation to finish it: a rollback
-- that could not reach the source to give its logins back. stuck_seconds
-- is how long since the project's last operation ended.
-- name: ListStuckProjects :many
-- tenant: system - the stuck-move sweep and the alerts repair or report every organisation's projects.
SELECT p.* FROM projects p
WHERE p.deleted_at IS NULL
  AND p.status IN ('moving', 'promoting', 'demoting', 'upgrading')
  AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.project_id = p.id AND o.status IN ('queued', 'running'))
  AND coalesce((SELECT max(o.finished_at) FROM operations o WHERE o.project_id = p.id), p.created_at)
      < now() - make_interval(secs => @stuck_seconds::float8)
ORDER BY p.created_at;

-- ListProjectsLeftWithoutOperation finds live projects in any status but
-- active (paused and archived projects are active, with a lifecycle) that
-- no queued or running operation will move on: a move-like rollback that
-- could not unfreeze, or a create, restore or delete whose operation ended
-- without its project's final status, or one left in error. stuck_seconds
-- is how long since the project's last operation ended. For the
-- project_stuck alert; RepairStuckMoves repairs only ListStuckProjects'.
-- name: ListProjectsLeftWithoutOperation :many
-- tenant: system - the project_stuck alert reports every organisation's projects.
SELECT p.* FROM projects p
WHERE p.deleted_at IS NULL
  AND p.status NOT IN ('active', 'deleted')
  AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.project_id = p.id AND o.status IN ('queued', 'running'))
  AND coalesce((SELECT max(o.finished_at) FROM operations o WHERE o.project_id = p.id), p.created_at)
      < now() - make_interval(secs => @stuck_seconds::float8)
ORDER BY p.created_at;

-- MoveNameInUse reports whether a queued or running operation owns the
-- move objects named after it (logical.Name: its id's first 16 hex digits).
-- name: MoveNameInUse :one
-- tenant: system - the move sweep checks replication objects on every instance against the operations still to finish.
SELECT EXISTS (SELECT 1 FROM operations
  WHERE status IN ('queued', 'running') AND starts_with(replace(id::text, '-', ''), @id_prefix::text));

-- MoveDatabaseBusy reports whether a queued or running operation is moving
-- a project's database named db_name to or from the instance.
-- name: MoveDatabaseBusy :one
-- tenant: system - the move sweep checks replication objects on every instance against the operations still to finish.
SELECT EXISTS (SELECT 1 FROM operations o JOIN projects p ON p.id = o.project_id
  WHERE o.status IN ('queued', 'running') AND p.db_name = @db_name::text
    AND @instance_id::text IN (o.params->>'source_instance', o.params->>'target_instance'));

-- name: ProjectDatabaseNames :many
-- tenant: system - the move sweep looks only at Xenibase's own databases on an instance.
SELECT DISTINCT db_name FROM projects WHERE db_name = ANY(@names::text[]);
