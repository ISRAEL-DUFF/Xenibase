-- name: InsertReadReplica :one
-- tenant: system - read replicas of a project the caller already resolved.
INSERT INTO read_replicas (id, project_id, instance_member_id, node_id, region_id, size, hot_standby_feedback, max_standby_delay_s)
VALUES (@id, @project_id, @instance_member_id, @node_id, sqlc.narg(region_id), @size, @hot_standby_feedback, @max_standby_delay_s)
RETURNING *;

-- name: SetReplicaSettings :one
-- tenant: system - read replicas of a project the caller already resolved.
UPDATE read_replicas SET hot_standby_feedback = @hot_standby_feedback, max_standby_delay_s = @max_standby_delay_s, updated_at = now()
WHERE id = @id AND project_id = @project_id AND deleted_at IS NULL
RETURNING *;

-- name: SetReplicaSettingsApplied :exec
-- tenant: system - read replicas of a project the caller already resolved.
UPDATE read_replicas SET settings_applied_at = CASE WHEN sqlc.narg(error)::text IS NULL THEN now() ELSE settings_applied_at END,
  settings_error = sqlc.narg(error)
WHERE id = @id;

-- name: GetReadReplica :one
-- tenant: system - read replicas of a project the caller already resolved.
SELECT * FROM read_replicas WHERE id = @id;

-- name: ListProjectReplicas :many
-- tenant: system - read replicas of a project the caller already resolved.
SELECT r.*, n.name AS node_name, n.region AS node_region, m.host, m.port, m.state AS member_state
FROM read_replicas r
JOIN nodes n ON n.id = r.node_id
LEFT JOIN instance_members m ON m.id = r.instance_member_id AND m.deleted_at IS NULL
WHERE r.project_id = @project_id AND r.deleted_at IS NULL
ORDER BY r.created_at, r.id;

-- name: CountProjectReplicas :one
-- tenant: system - read replicas of a project the caller already resolved.
SELECT count(*)::int FROM read_replicas WHERE project_id = @project_id AND deleted_at IS NULL;

-- name: SetReplicaStatus :exec
-- tenant: system - read replicas of a project the caller already resolved.
UPDATE read_replicas SET status = @status, error = sqlc.narg(error), updated_at = now() WHERE id = @id;

-- name: SetReplicaLag :exec
-- tenant: system - the replica watcher, for a replica it already resolved.
UPDATE read_replicas SET status = @status, in_rotation = @in_rotation, lag_bytes = sqlc.narg(lag_bytes), lag_ms = sqlc.narg(lag_ms),
  rotation_changed_at = CASE WHEN in_rotation <> @in_rotation THEN now() ELSE rotation_changed_at END, updated_at = now()
WHERE id = @id AND deleted_at IS NULL;

-- name: SetReplicaDetached :exec
-- tenant: system - read replicas of a project the caller already resolved.
UPDATE read_replicas SET status = 'detached', in_rotation = false, detached_project_id = @detached_project_id,
  deleted_at = now(), updated_at = now()
WHERE id = @id;

-- name: DeleteReadReplica :exec
-- tenant: system - read replicas of a project the caller already resolved.
UPDATE read_replicas SET status = 'deleted', in_rotation = false, deleted_at = now(), updated_at = now() WHERE id = @id;

-- name: SetMemberReplica :exec
-- tenant: system - HA members of an instance the caller already resolved.
UPDATE instance_members SET replica = true WHERE id = @id;

-- name: InstanceReplicas :many
-- tenant: system - the replica watcher: an instance's live replicas.
SELECT r.* FROM read_replicas r JOIN projects p ON p.id = r.project_id
WHERE p.instance_id = @instance_id AND r.deleted_at IS NULL
ORDER BY r.created_at, r.id;

-- name: PoolerReplicaRoutes :many
-- tenant: system - the poolers: every live read replica, whether it is in
-- rotation, and the address the pooler reaches it at.
SELECT p.db_name, r.id, r.in_rotation, n.region, COALESCE(m.host, n.private_addr)::text AS host, COALESCE(m.port, 0)::int AS port
FROM read_replicas r
JOIN projects p ON p.id = r.project_id
JOIN nodes n ON n.id = r.node_id
LEFT JOIN instance_members m ON m.id = r.instance_member_id AND m.deleted_at IS NULL
WHERE r.deleted_at IS NULL AND p.deleted_at IS NULL
ORDER BY p.db_name, r.created_at, r.id;

-- name: HourlyReplicas :many
-- tenant: system - usage recording; rows carry org_id.
-- Each read replica's size (its instance's), for each hour from @from_ts to
-- @last_hour it existed, with the fraction of the hour it did.
SELECT r.project_id, p.org_id, o.plan_id, g.h::timestamptz AS period_start,
       COALESCE(i.cpu_limit, 0)::float8 AS cpus, COALESCE(i.mem_limit_mb, 0)::int AS mem_mb, COALESCE(i.volume_gb, 0)::int AS disk_gb,
       (extract(epoch FROM LEAST(g.h + '1 hour'::interval, COALESCE(r.deleted_at, 'infinity'::timestamptz), COALESCE(p.deleted_at, 'infinity'::timestamptz)) - GREATEST(g.h, r.created_at)) / 3600)::float8 AS fraction
FROM read_replicas r
JOIN projects p ON p.id = r.project_id
JOIN instances i ON i.id = p.instance_id
JOIN organizations o ON o.id = p.org_id
CROSS JOIN generate_series(@from_ts::timestamptz, @last_hour::timestamptz, '1 hour'::interval) AS g(h)
WHERE r.created_at < g.h + '1 hour'::interval AND (r.deleted_at IS NULL OR r.deleted_at > g.h)
  AND (p.deleted_at IS NULL OR p.deleted_at > g.h);

-- name: GetInstanceMember :one
-- tenant: system - HA members of an instance the caller already resolved.
SELECT * FROM instance_members WHERE id = @id;

-- name: MarkReplicaDetaching :execrows
-- tenant: system - read replicas of a project the caller already resolved.
UPDATE read_replicas SET status = 'detaching', in_rotation = false, updated_at = now()
WHERE id = @id AND project_id = @project_id AND deleted_at IS NULL AND status IN ('streaming', 'lagging', 'down');

-- name: UnmarkReplicaDetaching :exec
-- tenant: system - read replicas of a project the caller already resolved.
UPDATE read_replicas SET status = 'down', updated_at = now() WHERE id = @id AND status = 'detaching' AND deleted_at IS NULL;

-- name: DeleteInstanceReplicas :exec
-- tenant: system - an instance being removed: its projects' replicas go with it.
UPDATE read_replicas SET status = 'deleted', in_rotation = false, deleted_at = now(), updated_at = now()
WHERE deleted_at IS NULL AND project_id IN (SELECT id FROM projects WHERE instance_id = @instance_id);
