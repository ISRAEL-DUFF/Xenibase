-- name: UpsertNode :one
INSERT INTO nodes (name, private_addr, role, pg_admin_secret, capacity)
VALUES (@name, @private_addr, @role, @pg_admin_secret, '{}')
ON CONFLICT (name) DO UPDATE
SET private_addr = EXCLUDED.private_addr, pg_admin_secret = EXCLUDED.pg_admin_secret
RETURNING *;

-- name: SetNodeAdminSecret :exec
UPDATE nodes SET pg_admin_secret = @pg_admin_secret WHERE id = @id;

-- name: UpsertSharedInstance :one
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
INSERT INTO instances (node_id, kind, pg_version, port, admin_host, admin_port, status)
VALUES (@node_id, 'shared', @pg_version, @port, sqlc.narg(admin_host), sqlc.narg(admin_port), 'running')
ON CONFLICT (node_id) WHERE kind = 'shared' AND deleted_at IS NULL DO UPDATE
SET pg_version = EXCLUDED.pg_version, port = EXCLUDED.port,
    admin_host = EXCLUDED.admin_host, admin_port = EXCLUDED.admin_port, status = 'running'
RETURNING *;

-- PickSharedInstance chooses the running shared instance hosting the fewest
-- live projects.
-- name: PickSharedInstance :one
-- tenant: system - platform infrastructure (nodes, instances, placement).
-- An organisation with its own shared clusters uses only those; every
-- other organisation uses only the untagged ones (V2 s10.5).
SELECT i.* FROM instances i
JOIN nodes n ON n.id = i.node_id
WHERE i.kind = 'shared' AND i.status = 'running' AND n.status = 'healthy' AND n.lifecycle = 'active' AND n.role IN ('shared', 'both')
  AND i.deleted_at IS NULL AND i.pg_version = @pg_version AND n.region = @region
  AND CASE WHEN EXISTS (SELECT 1 FROM instances x WHERE x.kind = 'shared' AND x.deleted_at IS NULL AND x.org_id = @org_id)
           THEN i.org_id = @org_id ELSE i.org_id IS NULL END
ORDER BY (SELECT count(*) FROM projects p WHERE p.instance_id = i.id AND p.deleted_at IS NULL), i.created_at
LIMIT 1;

-- GetInstanceTarget is how to reach an instance: host (poolers, agents)
-- defaults to the node's private address; the admin credential is the
-- instance's own (dedicated) or the node's (registered shared cluster).
-- name: GetInstanceTarget :one
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
SELECT i.id, i.kind, i.port, i.admin_host, i.admin_port, i.status,
       COALESCE(i.host, n.private_addr)::text AS host, i.admin_secret,
       n.id AS node_id, n.name AS node_name, n.private_addr, n.pg_admin_secret
FROM instances i JOIN nodes n ON n.id = i.node_id
WHERE i.id = @id;

-- name: GetInstance :one
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
SELECT * FROM instances WHERE id = @id;

-- name: InsertInstance :one
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
INSERT INTO instances (id, node_id, kind, pg_version, port, cpu_limit, mem_limit_mb, volume_gb, profile, walg_prefix, status, max_connections)
VALUES (@id, @node_id, @kind, @pg_version, 5432, @cpu_limit, @mem_limit_mb, @volume_gb, @profile, sqlc.narg(walg_prefix), 'provisioning',
  COALESCE(sqlc.narg(max_connections)::int, 100))
RETURNING *;

-- name: SetInstanceAdminSecret :exec
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
UPDATE instances SET admin_secret = @admin_secret WHERE id = @id;

-- name: SetInstanceRunning :exec
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
UPDATE instances
SET status = 'running', container_id = @container_id, host = @host, port = @port,
    admin_host = sqlc.narg(admin_host), admin_port = sqlc.narg(admin_port), error = NULL
WHERE id = @id;

-- name: SetInstanceStatus :exec
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
UPDATE instances SET status = @status, error = sqlc.narg(error) WHERE id = @id;

-- name: MarkInstanceDeleted :exec
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
UPDATE instances SET status = 'deleted', deleted_at = now() WHERE id = @id;

-- name: ListNodeInstances :many
-- tenant: system - platform infrastructure (nodes, instances, placement).
SELECT i.*, (SELECT count(*) FROM projects p WHERE p.instance_id = i.id AND p.deleted_at IS NULL)::int AS projects
FROM instances i
WHERE i.node_id = @node_id AND i.deleted_at IS NULL
ORDER BY i.created_at;

-- PickDedicatedNode chooses the healthy node with an agent that allows
-- dedicated instances and runs the fewest.
-- name: PickDedicatedNode :one
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
SELECT n.* FROM nodes n
WHERE n.role IN ('dedicated', 'both') AND n.status = 'healthy' AND n.lifecycle = 'active' AND n.agent_cert_fp IS NOT NULL
  AND n.region = @region
ORDER BY (SELECT count(*) FROM instances i WHERE i.node_id = n.id AND i.kind = 'dedicated' AND i.deleted_at IS NULL), n.created_at
LIMIT 1;

-- ListOrphanedInstances finds instances nothing uses and nothing will finish
-- setting up: dedicated instances no live project points at, and shared
-- clusters whose failed creation could not be rolled back (status 'error',
-- see rollbackSharedCluster). Both are what a rollback leaves when it could
-- not reach the node. The grace period keeps it clear of instances a create
-- or restore is still setting up.
-- name: ListOrphanedInstances :many
-- tenant: system - platform infrastructure (nodes, instances, placement).
SELECT i.* FROM instances i
WHERE i.deleted_at IS NULL
  AND i.created_at < now() - interval '5 minutes'
  AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.instance_id = i.id AND p.deleted_at IS NULL)
  -- A demoted project's stopped instance, kept as a rollback option.
  AND NOT EXISTS (SELECT 1 FROM retired_databases r WHERE r.instance_id = i.id AND r.dropped_at IS NULL)
  AND (i.kind = 'dedicated' OR (i.kind = 'shared' AND i.status = 'error'))
ORDER BY i.created_at;

-- name: ListInstanceSummaries :many
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
SELECT i.id, i.kind, i.profile, i.cpu_limit, i.mem_limit_mb, i.volume_gb, i.status, i.error, i.pg_version,
       i.pg_release, i.pg_release_available, i.ha_enabled, i.pitr_days, i.max_connections,
       n.id AS node_id, n.name AS node_name
FROM instances i JOIN nodes n ON n.id = i.node_id
WHERE i.deleted_at IS NULL;

-- name: SharedInstanceOnNode :one
-- tenant: system - provisioning workers and the agents' infrastructure; instances are resolved from a project the request already authorized.
SELECT * FROM instances WHERE node_id = @node_id AND kind = 'shared' AND deleted_at IS NULL;

-- name: SetInstanceOrg :execrows
-- tenant: system - the platform admin assigns a shared cluster to one organisation.
UPDATE instances SET org_id = sqlc.narg(org_id) WHERE id = @id AND kind = 'shared' AND deleted_at IS NULL;

-- name: SharedInstanceForeignProjects :one
-- tenant: system - projects of other organisations already on a shared cluster.
SELECT count(*)::int FROM projects p WHERE p.instance_id = @instance_id AND p.deleted_at IS NULL AND p.org_id <> @org_id;

-- name: OrgSharedInstances :many
-- tenant: system - the shared clusters tagged with one organisation.
SELECT i.*, n.name AS node_name FROM instances i JOIN nodes n ON n.id = i.node_id
WHERE i.kind = 'shared' AND i.deleted_at IS NULL AND i.org_id = @org_id;
