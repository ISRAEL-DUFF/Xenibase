-- name: InstancesToCheckRelease :many
-- tenant: system - the maintenance sweep reads every running instance.
SELECT * FROM instances
WHERE deleted_at IS NULL AND status = 'running'
  AND (release_checked_at IS NULL OR release_checked_at < @before::timestamptz)
ORDER BY release_checked_at NULLS FIRST, created_at;

-- name: SetInstanceRelease :exec
-- tenant: system - the maintenance sweep records what agents report.
UPDATE instances SET pg_release = sqlc.narg(pg_release), pg_release_available = sqlc.narg(pg_release_available),
  release_checked_at = now()
WHERE id = @id;

-- InstancesBehind lists running instances whose image has another release;
-- the caller keeps the ones where it is newer. Dedicated instances go
-- first: each restart there pauses one project, not a cluster's worth. An
-- instance whose last upgrade to that release failed is left out, so the
-- sweep moves on to the others; a newer release, or the admin's Run now,
-- tries it again.
-- name: InstancesBehind :many
-- tenant: system - the maintenance sweep reads every running instance.
SELECT * FROM instances i
WHERE i.deleted_at IS NULL AND i.status = 'running'
  AND i.pg_release IS NOT NULL AND i.pg_release_available IS NOT NULL AND i.pg_release <> i.pg_release_available
  AND NOT EXISTS (
    SELECT 1 FROM (
      SELECT m.error, m.to_release FROM minor_upgrades m
      WHERE m.instance_id = i.id AND m.finished_at IS NOT NULL
      ORDER BY m.started_at DESC LIMIT 1
    ) last
    WHERE last.error IS NOT NULL AND last.to_release = i.pg_release_available)
ORDER BY i.kind = 'shared', i.created_at;

-- InstanceBusy reports whether a project on the instance is in the middle
-- of something (a move, a restore, a queued operation) that a restart
-- would get in the way of.
-- name: InstanceBusy :one
-- tenant: system - the maintenance sweep checks every project on the instance.
SELECT EXISTS (
  SELECT 1 FROM projects p
  WHERE p.instance_id = @instance_id AND p.deleted_at IS NULL
    AND (p.status <> 'active' OR EXISTS (
      SELECT 1 FROM operations o WHERE o.project_id = p.id AND o.status IN ('queued', 'running')))
)::bool;

-- name: MaintenanceProjects :many
-- tenant: system - the maintenance sweep pauses every project on the instance.
SELECT * FROM projects WHERE instance_id = @instance_id AND deleted_at IS NULL ORDER BY created_at;

-- name: StartMinorUpgrade :one
-- tenant: system - the maintenance sweep records its own runs.
INSERT INTO minor_upgrades (instance_id, from_release, to_release) VALUES (@instance_id, @from_release, @to_release)
RETURNING *;

-- name: FinishMinorUpgrade :exec
-- tenant: system - the maintenance sweep records its own runs.
UPDATE minor_upgrades SET finished_at = now(), pause_ms = sqlc.narg(pause_ms), error = sqlc.narg(error) WHERE id = @id;

-- name: LatestMinorUpgrades :many
-- tenant: system - platform maintenance history for the admin console.
SELECT m.*, i.kind, n.name AS node_name
FROM minor_upgrades m JOIN instances i ON i.id = m.instance_id JOIN nodes n ON n.id = i.node_id
ORDER BY m.started_at DESC LIMIT @lim;

-- name: GetMinorUpgrade :one
-- tenant: system - platform maintenance history for the admin console.
SELECT m.*, i.kind, n.name AS node_name
FROM minor_upgrades m JOIN instances i ON i.id = m.instance_id JOIN nodes n ON n.id = i.node_id
WHERE m.id = @id;
