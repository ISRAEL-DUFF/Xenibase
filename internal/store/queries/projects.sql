-- name: InsertProject :one
INSERT INTO projects (id, org_id, name, slug, db_name, owner_role, scram_verifier, tier, instance_id, status, settings, description, created_by, region, data_residency)
VALUES (@id, @org_id, @name, @slug, @db_name, @owner_role, @scram_verifier, @tier, @instance_id, 'provisioning', @settings, sqlc.narg(description), sqlc.narg(created_by), @region, @data_residency)
RETURNING *;

-- name: GetProject :one
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
SELECT * FROM projects WHERE id = @id;

-- name: GetLiveProjectForUpdate :one
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
SELECT * FROM projects WHERE id = @id AND deleted_at IS NULL FOR UPDATE;

-- name: ListLiveProjects :many
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
SELECT * FROM projects
WHERE deleted_at IS NULL
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
ORDER BY created_at DESC, id DESC
LIMIT @max_rows;

-- name: SetProjectStatus :exec
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
UPDATE projects SET status = @status WHERE id = @id;

-- name: SetProjectIntegratorRoles :exec
-- tenant: system - the platform admin's integrator-role bootstrap on a dedicated instance.
UPDATE projects SET integrator_roles = sqlc.narg(integrator_roles)::text[] WHERE id = @id;

-- name: SetProjectVerifier :exec
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
UPDATE projects SET scram_verifier = @scram_verifier WHERE id = @id;

-- name: SoftDeleteProject :exec
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
UPDATE projects SET status = @status, deleted_at = now() WHERE id = @id;

-- PoolerRoutes lists every project the pooler should route to, with the
-- backend address the pooler uses and the SCRAM verifier for its auth file.
-- A suspended organisation's projects get no routes (V2 s10.8).
-- name: PoolerRoutes :many
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
SELECT p.db_name, p.alias_db_name, p.owner_role, p.scram_verifier, p.legacy_owner_role, p.legacy_scram_verifier,
       p.settings, COALESCE(i.host, n.private_addr)::text AS host, i.port, p.lifecycle,
       p.region, p.forward_region, p.forward_until
FROM projects p
JOIN instances i ON i.id = p.instance_id
JOIN nodes n ON n.id = i.node_id
JOIN organizations o ON o.id = p.org_id
WHERE p.deleted_at IS NULL
  AND p.status IN ('provisioning', 'active', 'promoting', 'demoting', 'moving', 'upgrading', 'restoring')
  AND o.status <> 'suspended'
ORDER BY p.db_name;

-- name: UpdateProjectMeta :one
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
UPDATE projects SET name = @name, description = sqlc.narg(description), settings = @settings
WHERE id = @id
RETURNING *;

-- name: ProjectsToRename :many
-- V1 projects whose backend database still has a descriptive name (V2 s10.2).
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
SELECT * FROM projects
WHERE deleted_at IS NULL AND status = 'active' AND db_name !~ '^p_[a-z2-7]{10}$'
ORDER BY created_at;

-- name: SetProjectBackendName :exec
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
UPDATE projects SET db_name = @db_name, alias_db_name = sqlc.narg(alias_db_name) WHERE id = @id;

-- name: SwitchProjectCredentials :exec
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
UPDATE projects SET owner_role = @owner_role, scram_verifier = @scram_verifier,
  legacy_owner_role = sqlc.narg(legacy_owner_role), legacy_scram_verifier = sqlc.narg(legacy_scram_verifier),
  legacy_until = sqlc.narg(legacy_until)
WHERE id = @id;

-- name: ProjectsLegacyExpired :many
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
SELECT * FROM projects
WHERE deleted_at IS NULL AND legacy_until IS NOT NULL AND legacy_until <= @now::timestamptz
ORDER BY legacy_until;

-- name: ClearProjectLegacy :exec
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
UPDATE projects SET legacy_owner_role = NULL, legacy_scram_verifier = NULL, legacy_until = NULL, alias_db_name = NULL
WHERE id = @id;

-- name: DBNameTaken :one
-- A name in use as any project's backend database or alias.
-- tenant: system - provisioning workers and the poolers, or a project the request already authorized.
SELECT EXISTS (SELECT 1 FROM projects WHERE db_name = @name OR alias_db_name = @name);

-- name: GetEditorPreferences :one
SELECT migration_format FROM editor_preferences WHERE project_id = @project_id AND user_id = @user_id AND org_id = @org_id;

-- name: PutEditorPreferences :exec
INSERT INTO editor_preferences (project_id, user_id, org_id, migration_format)
VALUES (@project_id, @user_id, @org_id, @migration_format)
ON CONFLICT (project_id, user_id) DO UPDATE SET migration_format = EXCLUDED.migration_format, org_id = EXCLUDED.org_id, updated_at = now();
