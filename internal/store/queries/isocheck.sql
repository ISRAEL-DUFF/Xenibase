-- name: ListLiveSharedInstances :many
-- tenant: system - the isolation checker; shared clusters belong to no organisation.
SELECT i.id, i.node_id, n.name AS node_name, i.status
FROM instances i JOIN nodes n ON n.id = i.node_id
WHERE i.kind = 'shared' AND i.deleted_at IS NULL AND n.status <> 'removed'
ORDER BY n.name, i.created_at;

-- name: InstanceProjects :many
-- tenant: system - the platform's isolation checks.
-- Live projects whose database is on an instance.
SELECT p.id, p.db_name, p.owner_role, p.legacy_owner_role, p.integrator_roles, p.storage_state, o.status AS org_status
FROM projects p JOIN organizations o ON o.id = p.org_id
WHERE p.instance_id = @instance_id AND p.deleted_at IS NULL AND p.status NOT IN ('provisioning', 'deleting', 'error')
ORDER BY p.db_name;

-- name: LatestIsolationChecks :many
-- tenant: system - the platform's isolation checks.
-- The newest isolation_check operation per instance.
SELECT DISTINCT ON (params->>'instance_id') (params->>'instance_id')::uuid AS instance_id, id, status, error, created_at, finished_at
FROM operations
WHERE kind = 'isolation_check'
ORDER BY params->>'instance_id', created_at DESC;
