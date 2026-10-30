-- Capacity automation (V3 §5): proposals, provisioned nodes, drains and
-- rebalancing, empty nodes.

-- name: CapacityNodes :many
-- tenant: system - the capacity planner counts every node's instances and projects.
-- Database nodes in service with what runs on them, for the planner.
SELECT n.*,
  (SELECT count(*) FROM instances i WHERE i.node_id = n.id AND i.deleted_at IS NULL)::int AS instances,
  (SELECT count(*) FROM instances i WHERE i.node_id = n.id AND i.deleted_at IS NULL AND i.kind = 'dedicated')::int AS dedicated_instances,
  (SELECT count(*) FROM instances i JOIN projects p ON p.instance_id = i.id AND p.deleted_at IS NULL
     WHERE i.node_id = n.id AND i.deleted_at IS NULL)::int AS projects,
  (SELECT coalesce(sum(i.cpu_limit), 0) FROM instances i WHERE i.node_id = n.id AND i.deleted_at IS NULL AND i.kind = 'dedicated')::float8 AS dedicated_cpus,
  (SELECT coalesce(sum(i.mem_limit_mb), 0) FROM instances i WHERE i.node_id = n.id AND i.deleted_at IS NULL)::bigint AS reserved_mem_mb,
  (SELECT coalesce(sum(i.volume_gb), 0) FROM instances i WHERE i.node_id = n.id AND i.deleted_at IS NULL AND i.kind = 'dedicated')::bigint AS dedicated_disk_gb,
  EXISTS (SELECT 1 FROM instances i WHERE i.node_id = n.id AND i.kind = 'shared' AND i.status = 'running' AND i.deleted_at IS NULL AND i.org_id IS NULL) AS has_shared_cluster
FROM nodes n
WHERE n.status <> 'removed' AND n.role <> 'pooler'
ORDER BY n.region, n.created_at;

-- name: NodeMetricSeries :many
-- tenant: system - node metrics belong to the platform, not an organisation.
-- A node metric's points since a time, oldest first, at a resolution.
SELECT ts, value FROM metric_points
WHERE scope = 'node' AND scope_id = @node_id AND metric = @metric AND resolution = @resolution AND ts >= @since
ORDER BY ts;

-- name: InsertProvisionedNode :one
-- A node for a server Xenibase is creating; its agent registers with the
-- token in the server's cloud-init.
INSERT INTO nodes (name, private_addr, role, capacity, registration_token, registration_expires_at,
  provider, region, server_type, monthly_cost_minor, cost_currency, lifecycle)
VALUES (@name, @private_addr, @role, '{}', @registration_token, @registration_expires_at,
  @provider, @region, @server_type, @monthly_cost_minor, @cost_currency, 'provisioning')
RETURNING *;

-- name: SetNodeServer :exec
UPDATE nodes SET provider_server_id = @provider_server_id, private_addr = @private_addr WHERE id = @id;

-- name: SetNodePrivateAddr :exec
UPDATE nodes SET private_addr = @private_addr WHERE id = @id;

-- name: SetNodeLifecycle :one
UPDATE nodes SET lifecycle = @lifecycle WHERE id = @id AND status <> 'removed' RETURNING *;

-- name: SetNodeCost :one
UPDATE nodes SET monthly_cost_minor = sqlc.narg(monthly_cost_minor), cost_currency = @cost_currency,
  server_type = coalesce(sqlc.narg(server_type), server_type), region = coalesce(sqlc.narg(region), region),
  keep = coalesce(sqlc.narg(keep), keep)
WHERE id = @id AND status <> 'removed' RETURNING *;

-- name: SetNodeEmptySince :exec
UPDATE nodes SET empty_since = sqlc.narg(empty_since) WHERE id = @id;

-- name: BudgetCostTotals :many
-- What counts against the capacity budget, by currency: the monthly cost
-- of every node in service, and of proposals approved or provisioning
-- whose node isn't one of those yet (a started proposal has no node until
-- its operation runs). One statement, so one snapshot: a proposal getting
-- its node meanwhile counts once.
SELECT c.currency, coalesce(sum(c.monthly_minor), 0)::bigint AS monthly_minor
FROM (
  SELECT n.cost_currency AS currency, n.monthly_cost_minor AS monthly_minor
  FROM nodes n WHERE n.status <> 'removed' AND n.monthly_cost_minor IS NOT NULL
  UNION ALL
  SELECT p.currency, p.monthly_cost_minor
  FROM capacity_proposals p
  WHERE p.status IN ('approved', 'provisioning')
    AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.id = p.node_id AND n.status <> 'removed' AND n.monthly_cost_minor IS NOT NULL)
) c
GROUP BY c.currency;

-- name: LockCapacityBudget :exec
-- Holds the budget until the transaction ends, so a budget check and the
-- start it allows are one step across Evaluate passes and HostFor calls
-- on every server. A waiter gives up after 30 seconds rather than hold
-- its connection indefinitely.
SELECT set_config('lock_timeout', '30s', true), pg_advisory_xact_lock(hashtext('xenibase_capacity_budget'));

-- name: NextNodeNumber :one
SELECT (count(*) + 1)::int FROM nodes WHERE name LIKE @prefix::text || '%';

-- name: InsertCapacityProposal :one
INSERT INTO capacity_proposals (region, tier, reason, provider, server_type, location, monthly_cost_minor, currency, auto)
VALUES (@region, @tier, @reason, @provider, @server_type, @location, @monthly_cost_minor, @currency, @auto)
ON CONFLICT (region, tier) WHERE status IN ('pending', 'approved', 'provisioning') DO NOTHING
RETURNING *;

-- name: GetCapacityProposal :one
SELECT * FROM capacity_proposals WHERE id = @id;

-- name: ListCapacityProposals :many
SELECT * FROM capacity_proposals ORDER BY created_at DESC LIMIT 100;

-- name: OpenCapacityProposal :one
SELECT * FROM capacity_proposals WHERE region = @region AND tier = @tier AND status IN ('pending', 'approved', 'provisioning');

-- name: DecideCapacityProposal :one
UPDATE capacity_proposals SET status = @status, decided_by = sqlc.narg(decided_by), decided_at = now(), updated_at = now()
WHERE id = @id AND status = 'pending' RETURNING *;

-- name: StartCapacityProposal :one
UPDATE capacity_proposals SET status = 'provisioning', operation_id = @operation_id, updated_at = now()
WHERE id = @id AND status IN ('pending', 'approved') RETURNING *;

-- name: SetCapacityProposalNode :exec
UPDATE capacity_proposals SET node_id = @node_id, updated_at = now() WHERE id = @id;

-- name: FinishCapacityProposal :exec
UPDATE capacity_proposals SET status = @status, error = sqlc.narg(error), updated_at = now() WHERE id = @id;

-- name: InsertRebalanceMove :one
-- tenant: system - the platform's capacity planner moves any project.
INSERT INTO rebalance_moves (batch, kind, project_id, from_node, to_node, reason, status)
VALUES (@batch, @kind, @project_id, @from_node, sqlc.narg(to_node), @reason, @status)
ON CONFLICT (project_id) WHERE status IN ('proposed', 'approved', 'moving') DO NOTHING
RETURNING *;

-- name: ListRebalanceMoves :many
-- tenant: system - the platform admin's view of drains and rebalancing.
SELECT m.*, p.name AS project_name, p.org_id, fn.name AS from_name, tn.name AS to_name
FROM rebalance_moves m JOIN projects p ON p.id = m.project_id
JOIN nodes fn ON fn.id = m.from_node LEFT JOIN nodes tn ON tn.id = m.to_node
WHERE m.status IN ('proposed', 'approved', 'moving') OR m.updated_at > now() - interval '14 days'
ORDER BY m.created_at DESC LIMIT 500;

-- name: NextApprovedMove :one
-- tenant: system - the mover takes the oldest approved move.
SELECT * FROM rebalance_moves WHERE status = 'approved' ORDER BY created_at LIMIT 1;

-- name: MovingRebalanceMoves :many
-- tenant: system - moves in flight, to see when their operations end.
SELECT * FROM rebalance_moves WHERE status = 'moving';

-- name: SetRebalanceMove :exec
-- tenant: system - the mover records a move's progress.
UPDATE rebalance_moves SET status = @status, to_node = coalesce(sqlc.narg(to_node), to_node),
  operation_id = coalesce(sqlc.narg(operation_id), operation_id), error = sqlc.narg(error), updated_at = now()
WHERE id = @id;

-- name: DecideRebalanceBatch :execrows
-- tenant: system - the platform admin approves or rejects a batch.
UPDATE rebalance_moves SET status = @status, updated_at = now() WHERE batch = @batch AND status = 'proposed';

-- name: CancelNodeDrain :execrows
-- tenant: system - stopping a drain drops its moves not yet started.
UPDATE rebalance_moves SET status = 'skipped', updated_at = now(), error = 'the drain was stopped'
WHERE from_node = @from_node AND kind = 'drain' AND status IN ('proposed', 'approved');

-- name: NodeProjects :many
-- tenant: system - what a drain has to move off a node.
SELECT p.* FROM projects p JOIN instances i ON i.id = p.instance_id
WHERE i.node_id = @node_id AND p.deleted_at IS NULL AND i.deleted_at IS NULL
ORDER BY p.created_at;

-- name: NodeProjectSizes :many
-- tenant: system - each project on a node with its last measured size, for rebalancing.
SELECT p.id, p.org_id, p.tier, p.instance_id, i.pg_version, i.org_id AS cluster_org,
  coalesce((SELECT m.value FROM metric_points m WHERE m.scope = 'project' AND m.scope_id = p.id AND m.metric = 'size_bytes'
    ORDER BY m.ts DESC LIMIT 1), 0)::float8 AS size_bytes
FROM projects p JOIN instances i ON i.id = p.instance_id
WHERE i.node_id = @node_id AND p.deleted_at IS NULL AND p.status = 'active' AND i.deleted_at IS NULL
ORDER BY size_bytes DESC;

-- name: NodeOccupancy :many
-- tenant: system - what still holds each node: live projects, dedicated
-- instances, and copies a move or promotion keeps for a while.
SELECT n.id,
  (SELECT count(*) FROM projects p JOIN instances i ON i.id = p.instance_id
     WHERE i.node_id = n.id AND p.deleted_at IS NULL AND i.deleted_at IS NULL)::int AS projects,
  (SELECT count(*) FROM instances i WHERE i.node_id = n.id AND i.deleted_at IS NULL AND i.kind = 'dedicated')::int AS dedicated,
  (SELECT count(*) FROM retired_databases r JOIN instances i ON i.id = r.instance_id
     WHERE i.node_id = n.id AND r.dropped_at IS NULL)::int AS retired,
  (SELECT count(*) FROM instance_members m WHERE m.node_id = n.id AND m.deleted_at IS NULL)::int AS members,
  (SELECT count(*) FROM etcd_members e WHERE e.node_id = n.id)::int AS etcd
FROM nodes n
WHERE n.status <> 'removed' AND n.role <> 'pooler';

-- name: LockNodeForRetire :one
-- The empty-node sweep holds a node's row while it counts what is on it
-- and changes its lifecycle; a project placed on the node holds it FOR
-- SHARE (LockNodeForPlacement).
SELECT * FROM nodes WHERE id = @id AND status <> 'removed' FOR UPDATE;

-- name: LockNodeForPlacement :one
-- A project's placement on a node holds the node's row until it commits,
-- so the empty-node sweep either counts the project or has already taken
-- the node out (lifecycle 'retiring').
SELECT lifecycle FROM nodes WHERE id = @id AND status <> 'removed' FOR SHARE;

-- name: NodeOccupancyOf :one
-- tenant: system - the empty-node sweep counts again what holds one node, under its lock.
SELECT
  (SELECT count(*) FROM projects p JOIN instances i ON i.id = p.instance_id
     WHERE i.node_id = @id AND p.deleted_at IS NULL AND i.deleted_at IS NULL)::int AS projects,
  (SELECT count(*) FROM instances i WHERE i.node_id = @id AND i.deleted_at IS NULL AND i.kind = 'dedicated')::int AS dedicated,
  (SELECT count(*) FROM retired_databases r JOIN instances i ON i.id = r.instance_id
     WHERE i.node_id = @id AND r.dropped_at IS NULL)::int AS retired,
  (SELECT count(*) FROM instance_members m WHERE m.node_id = @id AND m.deleted_at IS NULL)::int AS members,
  (SELECT count(*) FROM etcd_members e WHERE e.node_id = @id)::int AS etcd;

-- name: SetNodeRetiring :execrows
-- Takes an empty node out of placement before its server is deleted.
UPDATE nodes SET retiring_from = lifecycle, lifecycle = 'retiring', retiring_at = @retiring_at
WHERE id = @id AND lifecycle IN ('active', 'draining') AND status <> 'removed';

-- name: ReturnRetiringNode :execrows
-- Puts a retiring node back as it was: something was placed on it.
UPDATE nodes SET lifecycle = coalesce(retiring_from, 'active'), retiring_from = NULL, retiring_at = NULL, empty_since = NULL
WHERE id = @id AND lifecycle = 'retiring';

-- name: RetireNodeSharedClusters :exec
-- tenant: system - capacity automation retiring an empty node's shared clusters.
-- An empty node's shared clusters go with it.
UPDATE instances SET deleted_at = now(), status = 'deleted'
WHERE node_id = @node_id AND kind = 'shared' AND deleted_at IS NULL;

-- name: LockCapacityProposal :one
SELECT * FROM capacity_proposals WHERE id = @id FOR UPDATE;
