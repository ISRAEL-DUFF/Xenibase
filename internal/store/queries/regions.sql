-- Regions (V3 §6).

-- name: ListRegions :many
SELECT * FROM regions ORDER BY created_at, id;

-- name: GetRegion :one
SELECT * FROM regions WHERE id = @id;

-- name: EnsureRegion :exec
INSERT INTO regions (id, name) VALUES (@id, @name) ON CONFLICT DO NOTHING;

-- name: UpsertRegion :one
INSERT INTO regions (id, name, country, pooler_host, api_domain, provider, location, storage_target_id, copy_target_id, floating_ip_id, residency, status)
VALUES (@id, @name, @country, @pooler_host, @api_domain, @provider, @location, sqlc.narg(storage_target_id), sqlc.narg(copy_target_id), sqlc.narg(floating_ip_id), @residency, @status)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, country = EXCLUDED.country, pooler_host = EXCLUDED.pooler_host,
  api_domain = EXCLUDED.api_domain, provider = EXCLUDED.provider, location = EXCLUDED.location, storage_target_id = EXCLUDED.storage_target_id,
  copy_target_id = EXCLUDED.copy_target_id, floating_ip_id = EXCLUDED.floating_ip_id, residency = EXCLUDED.residency, status = EXCLUDED.status
RETURNING *;

-- name: SetRegionStatus :one
UPDATE regions SET status = @status WHERE id = @id RETURNING *;

-- name: RegionUsage :many
-- tenant: system - each region's nodes and live projects, for the regions list.
SELECT r.id,
  (SELECT count(*) FROM nodes n WHERE n.region = r.id AND n.status <> 'removed' AND n.role <> 'pooler')::int AS nodes,
  (SELECT count(*) FROM nodes n WHERE n.region = r.id AND n.status <> 'removed' AND n.role = 'pooler')::int AS pooler_hosts,
  (SELECT count(*) FROM projects p WHERE p.region = r.id AND p.deleted_at IS NULL)::int AS projects,
  (SELECT count(*) FROM projects p WHERE p.region = r.id AND p.deleted_at IS NULL AND p.data_residency)::int AS residency_projects,
  EXISTS (SELECT 1 FROM instances i JOIN nodes n ON n.id = i.node_id WHERE n.region = r.id AND i.kind = 'shared'
    AND i.status = 'running' AND i.deleted_at IS NULL AND i.org_id IS NULL) AS has_shared,
  EXISTS (SELECT 1 FROM nodes n WHERE n.region = r.id AND n.status = 'healthy' AND n.role IN ('dedicated', 'both')) AS has_dedicated
FROM regions r ORDER BY r.created_at, r.id;

-- name: SetProjectResidency :one
-- tenant: system - a project the request already authorized (owners only).
UPDATE projects SET data_residency = @data_residency WHERE id = @id AND deleted_at IS NULL RETURNING *;

-- name: SetProjectRegion :exec
-- tenant: system - a region move's cutover: the old hostname keeps routing for 30 days.
UPDATE projects SET forward_region = CASE WHEN region <> @region THEN region ELSE forward_region END,
  forward_until = CASE WHEN region <> @region THEN @forward_until ELSE forward_until END,
  region = @region
WHERE id = @id;

-- name: SetStorageTargetRegion :exec
-- tenant: system - the platform admin marks a platform target as in a region.
UPDATE storage_targets SET xenibase_region = sqlc.narg(xenibase_region) WHERE id = @id;

-- Early access to hidden regions.

-- name: RegionEarlyAccessOrgs :many
SELECT org_id FROM region_early_access WHERE region_id = @region_id ORDER BY created_at, org_id;

-- name: RegionHasEarlyAccess :one
SELECT EXISTS (SELECT 1 FROM region_early_access WHERE region_id = @region_id AND org_id = @org_id);

-- name: EarlyAccessRegionsForOrg :many
SELECT region_id FROM region_early_access WHERE org_id = @org_id;

-- name: ClearRegionEarlyAccess :exec
-- tenant: system - the platform admin replaces a region's whole list.
DELETE FROM region_early_access WHERE region_id = @region_id;

-- name: AddRegionEarlyAccess :exec
INSERT INTO region_early_access (region_id, org_id) VALUES (@region_id, @org_id) ON CONFLICT DO NOTHING;
