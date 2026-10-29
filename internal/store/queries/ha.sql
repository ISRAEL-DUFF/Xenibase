-- name: ListEtcdMembers :many
-- tenant: system - platform infrastructure (the etcd clusters, every region).
SELECT e.*, n.name AS node_name FROM etcd_members e JOIN nodes n ON n.id = e.node_id ORDER BY e.region, e.created_at, e.name;

-- name: ListRegionEtcdMembers :many
-- tenant: system - platform infrastructure (one region's etcd cluster).
SELECT e.*, n.name AS node_name FROM etcd_members e JOIN nodes n ON n.id = e.node_id
WHERE e.region = @region ORDER BY e.created_at, e.name;

-- name: GetEtcdMember :one
-- tenant: system - platform infrastructure (the etcd cluster).
SELECT e.*, n.name AS node_name FROM etcd_members e JOIN nodes n ON n.id = e.node_id WHERE e.node_id = @node_id;

-- name: InsertEtcdMember :exec
-- tenant: system - platform infrastructure (the etcd cluster).
INSERT INTO etcd_members (node_id, name, client_url, peer_url, region) VALUES (@node_id, @name, @client_url, @peer_url, @region);

-- name: SetEtcdMemberStatus :exec
-- tenant: system - platform infrastructure (the etcd cluster).
UPDATE etcd_members SET status = @status, error = sqlc.narg(error), checked_at = now() WHERE node_id = @node_id;

-- name: DeleteRegionEtcdMembers :exec
-- tenant: system - platform infrastructure (one region's etcd cluster).
DELETE FROM etcd_members WHERE region = @region;

-- name: DeleteEtcdMember :exec
-- tenant: system - platform infrastructure (a replaced etcd member).
DELETE FROM etcd_members WHERE node_id = @node_id;

-- name: SetInstanceEtcdRegion :exec
-- tenant: system - which region's etcd cluster an HA instance's Patroni uses.
UPDATE instances SET etcd_region = sqlc.narg(etcd_region) WHERE id = @id;

-- name: EtcdRegionInstances :many
-- tenant: system - the instances under Patroni whose state is in a region's etcd cluster.
SELECT * FROM instances WHERE patroni AND deleted_at IS NULL AND etcd_region = @region ORDER BY created_at;

-- name: InsertInstanceMember :one
-- tenant: system - HA members of an instance the caller already resolved.
INSERT INTO instance_members (id, instance_id, node_id, role) VALUES (@id, @instance_id, @node_id, @role)
ON CONFLICT (id) DO UPDATE SET deleted_at = NULL, node_id = EXCLUDED.node_id, role = EXCLUDED.role, updated_at = now()
RETURNING *;

-- name: ListInstanceMembers :many
-- tenant: system - HA members of an instance the caller already resolved.
SELECT m.*, n.name AS node_name, n.private_addr AS node_addr
FROM instance_members m JOIN nodes n ON n.id = m.node_id
WHERE m.instance_id = @instance_id AND m.deleted_at IS NULL
ORDER BY m.created_at, m.id;

-- name: SetMemberAddress :exec
-- tenant: system - HA members of an instance the caller already resolved.
UPDATE instance_members SET host = sqlc.narg(host), port = sqlc.narg(port), rest_host = sqlc.narg(rest_host),
  rest_port = sqlc.narg(rest_port), admin_host = sqlc.narg(admin_host), admin_port = sqlc.narg(admin_port), updated_at = now()
WHERE id = @id;

-- name: SetMemberState :exec
-- tenant: system - HA members of an instance the caller already resolved.
UPDATE instance_members SET role = @role, state = sqlc.narg(state), lag_bytes = sqlc.narg(lag_bytes),
  timeline = sqlc.narg(timeline), error = sqlc.narg(error), updated_at = now()
WHERE id = @id;

-- name: DeleteInstanceMember :exec
-- tenant: system - HA members of an instance the caller already resolved.
UPDATE instance_members SET deleted_at = now(), role = 'stopped' WHERE id = @id;

-- name: SetInstancePatroni :exec
-- tenant: system - HA settings of an instance the caller already resolved.
UPDATE instances SET patroni = @patroni, ha_enabled = @ha_enabled, sync_replication = @sync_replication,
  leader_member = sqlc.narg(leader_member), patroni_secret = sqlc.narg(patroni_secret)
WHERE id = @id;

-- name: SetInstanceHAEnabled :exec
-- tenant: system - HA settings of an instance the caller already resolved.
UPDATE instances SET ha_enabled = @ha_enabled WHERE id = @id;

-- name: SetInstanceSync :exec
-- tenant: system - HA settings of an instance the caller already resolved.
UPDATE instances SET sync_replication = @sync_replication WHERE id = @id;

-- SetInstanceLeader points the instance at its new leader: the node, the
-- member (the agent's container key), and the addresses.
-- name: SetInstanceLeader :exec
-- tenant: system - HA failover of an instance the caller already resolved.
UPDATE instances SET node_id = @node_id, leader_member = @leader_member, host = sqlc.narg(host), port = @port,
  admin_host = sqlc.narg(admin_host), admin_port = sqlc.narg(admin_port)
WHERE id = @id;

-- name: ListPatroniInstances :many
-- tenant: system - the HA leader watcher.
SELECT * FROM instances WHERE patroni AND deleted_at IS NULL ORDER BY created_at;

-- name: InsertFailoverEvent :one
-- tenant: system - the HA leader watcher.
INSERT INTO failover_events (instance_id, from_member, to_member, from_node, to_node, kind, duration_ms)
VALUES (@instance_id, sqlc.narg(from_member), sqlc.narg(to_member), sqlc.narg(from_node), sqlc.narg(to_node), @kind, sqlc.narg(duration_ms))
RETURNING *;

-- name: ListFailoverEvents :many
-- tenant: system - an instance the request already authorized.
SELECT f.*, fn.name AS from_node_name, tn.name AS to_node_name
FROM failover_events f LEFT JOIN nodes fn ON fn.id = f.from_node LEFT JOIN nodes tn ON tn.id = f.to_node
WHERE f.instance_id = @instance_id ORDER BY f.occurred_at DESC LIMIT @lim;

-- name: ProjectOnInstance :one
-- tenant: system - the HA leader watcher (one project per dedicated instance).
SELECT * FROM projects WHERE instance_id = @instance_id AND deleted_at IS NULL ORDER BY created_at LIMIT 1;

-- name: AvailabilitySummary :one
-- tenant: system - a project the request already authorized.
SELECT count(*) FILTER (WHERE NOT excluded)::int AS measured,
       count(*) FILTER (WHERE NOT available AND NOT excluded)::int AS unavailable
FROM availability_minutes WHERE project_id = @project_id AND minute >= @from_ts AND minute < @to_ts;

-- name: RecentOutageMinutes :many
-- tenant: system - a project the request already authorized.
SELECT * FROM availability_minutes
WHERE project_id = @project_id AND NOT available AND NOT excluded AND minute >= @since
ORDER BY minute DESC LIMIT @lim;

-- name: SetProbeVerifier :exec
-- tenant: system - the SLA probe login of a project the caller resolved.
UPDATE projects SET probe_verifier = sqlc.narg(probe_verifier) WHERE id = @id;

-- name: PoolerProbeUsers :many
-- tenant: system - the SLA probe logins the poolers must accept.
SELECT db_name, probe_verifier::text AS probe_verifier, region, forward_region, forward_until FROM projects
WHERE deleted_at IS NULL AND probe_verifier IS NOT NULL
  AND status IN ('provisioning', 'active', 'promoting', 'demoting', 'moving', 'upgrading', 'restoring')
ORDER BY db_name;

-- name: SLAProbeTargets :many
-- tenant: system - the SLA prober: every HA project.
SELECT p.*, i.patroni_secret, o.status AS org_status
FROM projects p JOIN instances i ON i.id = p.instance_id JOIN organizations o ON o.id = p.org_id
WHERE p.deleted_at IS NULL AND i.ha_enabled AND p.probe_verifier IS NOT NULL
ORDER BY p.id;

-- name: RecordAvailability :exec
-- tenant: system - the SLA prober.
INSERT INTO availability_minutes (project_id, minute, internal_ok, external_ok, excluded)
VALUES (@project_id, @minute, sqlc.narg(internal_ok), sqlc.narg(external_ok), @excluded)
ON CONFLICT (project_id, minute) DO UPDATE SET
  internal_ok = CASE WHEN EXCLUDED.internal_ok IS NULL THEN availability_minutes.internal_ok
                     ELSE coalesce(availability_minutes.internal_ok, true) AND EXCLUDED.internal_ok END,
  external_ok = CASE WHEN EXCLUDED.external_ok IS NULL THEN availability_minutes.external_ok
                     ELSE coalesce(availability_minutes.external_ok, true) AND EXCLUDED.external_ok END,
  excluded = availability_minutes.excluded OR EXCLUDED.excluded;

-- name: ApplyMaintenanceExclusions :execrows
-- tenant: system - the SLA prober: minutes inside maintenance announced 72 hours ahead are excluded (V3.1 4.2).
UPDATE availability_minutes a SET excluded = true, excluded_by = i.id
FROM incidents i, projects p
WHERE a.project_id = p.id AND a.minute >= @from_ts AND a.minute < @to_ts AND a.excluded_by IS NULL
  AND i.severity = 'maintenance' AND i.announced_at IS NOT NULL
  AND a.minute >= i.scheduled_start AND a.minute < i.scheduled_end
  AND i.announced_at <= a.minute - interval '72 hours'
  AND (i.cancelled_at IS NULL OR i.cancelled_at > a.minute)
  AND maintenance_covers(i, p);

-- name: AvailabilityExclusions :many
-- tenant: system - a project the request already authorized: excluded minutes by announcement.
SELECT i.id, i.title, i.scheduled_start, i.scheduled_end, count(*)::int AS minutes
FROM availability_minutes a JOIN incidents i ON i.id = a.excluded_by
WHERE a.project_id = @project_id AND a.minute >= @from_ts AND a.minute < @to_ts
GROUP BY i.id, i.title, i.scheduled_start, i.scheduled_end ORDER BY i.scheduled_start;

-- name: HAOnOtherEtcd :many
-- tenant: system - a region's projects under Patroni whose state is in another region's etcd cluster (V3.1 §3.3).
SELECT p.id, p.name, p.org_id, coalesce(nullif(i.etcd_region, ''), n.region)::text AS etcd_region
FROM projects p JOIN instances i ON i.id = p.instance_id JOIN nodes n ON n.id = i.node_id
WHERE p.region = @region AND p.deleted_at IS NULL AND i.deleted_at IS NULL AND i.patroni
  AND coalesce(nullif(i.etcd_region, ''), n.region) <> @region
ORDER BY p.created_at;

-- name: RegionPoolerHosts :many
-- tenant: system - platform infrastructure (a region's pooler hosts).
SELECT * FROM nodes WHERE role = 'pooler' AND status <> 'removed' AND region = @region ORDER BY name;

-- name: LatestRegionOperation :one
-- tenant: system - the newest platform operation of a kind for a region.
SELECT * FROM operations WHERE kind = @kind AND project_id IS NULL AND params->>'region' = @region::text
ORDER BY created_at DESC LIMIT 1;

-- name: SetInstanceSwitchover :exec
-- tenant: system - a switchover of an instance the caller already resolved.
UPDATE instances SET switchover_started_at = sqlc.narg(started_at) WHERE id = @id;
