-- Backend services (V4 §2): project refs, API keys, signing keys, and what
-- xenibase-edge reads.

-- name: GetProjectServices :one
-- tenant: system - a project the request already authorized.
SELECT * FROM project_services WHERE project_id = @project_id;

-- name: CreateProjectServices :one
-- tenant: system - a project the request already authorized.
INSERT INTO project_services (project_id, ref) VALUES (@project_id, @ref)
ON CONFLICT (project_id) DO NOTHING
RETURNING *;

-- name: SetServicesEnabled :one
-- tenant: system - a project the caller resolved.
UPDATE project_services SET enabled = @enabled,
  enabled_at = CASE WHEN @enabled::boolean THEN now() ELSE enabled_at END
WHERE project_id = @project_id
RETURNING *;

-- name: SetServicesRoles :exec
-- tenant: system - a project the caller resolved.
UPDATE project_services SET edge_verifier = @edge_verifier, login_verifiers = @login_verifiers, schema_version = @schema_version,
  roles_instance = @roles_instance
WHERE project_id = @project_id;

-- name: UpdateServicesSettings :one
-- tenant: system - a project the request already authorized.
UPDATE project_services SET cors_origins = @cors_origins, settings = @settings,
  exposed_schemas = @exposed_schemas, public_tables = @public_tables
WHERE project_id = @project_id
RETURNING *;

-- name: ServicesToReconcile :many
-- tenant: system - the services reconciler: enabled projects whose roles or xb_* schemas are behind or on another instance.
SELECT p.* FROM project_services s JOIN projects p ON p.id = s.project_id
WHERE s.enabled AND p.deleted_at IS NULL AND p.status = 'active' AND p.lifecycle = 'active'
  AND (s.schema_version < @schema_version OR s.roles_instance IS DISTINCT FROM p.instance_id)
ORDER BY p.id
LIMIT 50;

-- name: InsertAPIKey :one
-- tenant: system - a project the request already authorized.
INSERT INTO project_api_keys (project_id, kind, name, key_hash, prefix, display, created_by)
VALUES (@project_id, @kind, @name, @key_hash, @prefix, sqlc.narg(display), sqlc.narg(created_by))
RETURNING *;

-- name: ListAPIKeys :many
-- tenant: system - a project the request already authorized.
SELECT * FROM project_api_keys WHERE project_id = @project_id ORDER BY revoked_at IS NOT NULL, created_at;

-- name: RevokeAPIKey :one
-- tenant: system - a project the request already authorized.
UPDATE project_api_keys SET revoked_at = now()
WHERE id = @id AND project_id = @project_id AND revoked_at IS NULL
RETURNING *;

-- name: RevokeProjectAPIKeys :exec
-- tenant: system - a project the caller resolved (disabling services).
UPDATE project_api_keys SET revoked_at = now() WHERE project_id = @project_id AND revoked_at IS NULL;

-- name: TouchAPIKeys :exec
-- tenant: system - the edge's report of keys in use.
UPDATE project_api_keys SET last_used_at = greatest(coalesce(last_used_at, '-infinity'), @at::timestamptz)
WHERE id = ANY(@ids::uuid[]);

-- name: InsertJWTKey :one
-- tenant: system - a project the caller resolved.
INSERT INTO project_jwt_keys (id, project_id, kid, public_jwk, private_enc, status)
VALUES (@id, @project_id, @kid, @public_jwk, @private_enc, 'active')
RETURNING *;

-- name: ActiveJWTKey :one
-- tenant: system - a project the caller resolved.
SELECT * FROM project_jwt_keys WHERE project_id = @project_id AND status = 'active';

-- name: ProjectJWTKeys :many
-- tenant: system - a project the request already authorized.
SELECT * FROM project_jwt_keys WHERE project_id = @project_id AND status <> 'retired' ORDER BY created_at;

-- name: EdgeConfigChanges :many
-- tenant: system - xenibase-edge's configuration feed: every project with backend services changed since a point.
SELECT s.project_id, s.ref, s.enabled, s.cors_origins, s.settings, s.exposed_schemas, s.public_tables,
  s.config_version, s.changed_seq, (s.edge_verifier IS NOT NULL)::boolean AS edge_ready,
  s.storage_quota_bytes, s.upload_max_bytes, s.storage_egress_blocked, s.transforms_blocked,
  s.realtime_max_connections, s.realtime_messages_blocked,
  s.plan_timeout_ms, s.plan_rate_per_ip, s.plan_rate_per_key, s.api_requests_blocked, s.mau_blocked, s.mau_counted,
  EXISTS (SELECT 1 FROM read_replicas r WHERE r.project_id = p.id AND r.deleted_at IS NULL)::boolean AS has_replicas,
  p.db_name, p.region, p.data_residency, p.org_id, p.lifecycle, p.status, p.deleted_at, o.status AS org_status, o.plan_id,
  coalesce(b.capped, false)::boolean AS spend_capped
FROM project_services s JOIN projects p ON p.id = s.project_id JOIN organizations o ON o.id = p.org_id
LEFT JOIN billing_accounts b ON b.org_id = p.org_id
WHERE s.changed_seq > @since
ORDER BY s.changed_seq
LIMIT @lim;

-- name: EdgeConfigSeq :one
-- tenant: system - xenibase-edge's configuration feed position.
SELECT coalesce(max(changed_seq), 0)::bigint FROM project_services;

-- name: EdgeAPIKeys :many
-- tenant: system - xenibase-edge's configuration feed: the live keys' hashes.
SELECT project_id, id, kind, key_hash FROM project_api_keys
WHERE project_id = ANY(@project_ids::uuid[]) AND revoked_at IS NULL;

-- name: EdgeJWTKeys :many
-- tenant: system - xenibase-edge's configuration feed: public signing keys.
SELECT project_id, kid, public_jwk FROM project_jwt_keys
WHERE project_id = ANY(@project_ids::uuid[]) AND status <> 'retired';

-- name: ProjectServicesByRef :one
-- tenant: system - xenibase-edge names a project by its reference.
SELECT s.*, p.db_name FROM project_services s JOIN projects p ON p.id = s.project_id WHERE s.ref = @ref;

-- name: PoolerEdgeUsers :many
-- tenant: system - the edge logins the poolers must accept.
SELECT p.db_name, s.edge_verifier::text AS edge_verifier, s.login_verifiers, p.region, p.forward_region, p.forward_until
FROM project_services s JOIN projects p ON p.id = s.project_id
WHERE s.enabled AND s.edge_verifier IS NOT NULL AND p.deleted_at IS NULL
ORDER BY p.db_name;

-- name: InsertEdgeReport :execrows
-- tenant: system - xenibase-edge's usage and log reports, once per batch.
INSERT INTO edge_reports (batch_id, edge) VALUES (@batch_id, @edge) ON CONFLICT DO NOTHING;

-- name: PruneEdgeReports :execrows
-- tenant: system - housekeeping.
DELETE FROM edge_reports WHERE received_at < @before;

-- name: InsertRequestLogs :copyfrom
-- tenant: system - xenibase-edge's request logs.
INSERT INTO api_request_logs (project_id, at, request_id, method, path, status, latency_ms, role, user_id, key_id, ip, bytes_out)
VALUES (@project_id, @at, @request_id, @method, @path, @status, @latency_ms, @role, @user_id, @key_id, @ip, @bytes_out);

-- name: PruneRequestLogs :execrows
-- tenant: system - housekeeping: request logs are kept 7 days.
DELETE FROM api_request_logs WHERE at < @before;

-- name: ProjectRequestLogs :many
-- tenant: system - a project the request already authorized.
SELECT * FROM api_request_logs
WHERE project_id = @project_id AND (sqlc.narg(before)::bigint IS NULL OR id < sqlc.narg(before))
  AND status BETWEEN @status_min::int AND @status_max::int AND path LIKE @path_prefix::text || '%'
ORDER BY id DESC LIMIT @lim;

-- name: ProjectRequestLogsAfter :many
-- tenant: system - a project the request already authorized: logs newer than a cursor, oldest first (`xenibase logs api --follow`).
SELECT * FROM api_request_logs
WHERE project_id = @project_id AND id > @after::bigint
  AND status BETWEEN @status_min::int AND @status_max::int AND path LIKE @path_prefix::text || '%'
ORDER BY id LIMIT @lim;

-- name: LatestRequestLogID :one
-- tenant: system - a project the request already authorized: where a follow starts.
SELECT coalesce(max(id), 0)::bigint FROM api_request_logs WHERE project_id = @project_id;

-- name: ProjectUsageOwner :many
-- tenant: system - xenibase-edge's usage: each project's organisation and plan.
SELECT p.id, p.org_id, o.plan_id FROM projects p JOIN organizations o ON o.id = p.org_id
WHERE p.id = ANY(@project_ids::uuid[]);

-- name: StorageProjects :many
-- tenant: system - the storage sweep: running projects with backend services' storage, on or turned off (which keeps the files).
SELECT sqlc.embed(p), s.ref, s.storage_quota_bytes, s.upload_max_bytes, s.storage_egress_blocked, s.transforms_blocked
FROM project_services s JOIN projects p ON p.id = s.project_id
WHERE s.schema_version >= 4 AND p.deleted_at IS NULL AND p.status = 'active' AND p.lifecycle = 'active'
ORDER BY p.org_id, p.id;

-- name: UpsertProjectStorage :exec
-- tenant: system - the storage sweep's measurement.
INSERT INTO project_storage (project_id, bytes, objects, measured_at) VALUES (@project_id, @bytes, @objects, now())
ON CONFLICT (project_id) DO UPDATE SET bytes = EXCLUDED.bytes, objects = EXCLUDED.objects, measured_at = now();

-- name: OrgFileBytes :many
-- tenant: system - the storage sweep: each live project's measured files in an organisation.
SELECT ps.project_id, ps.bytes FROM project_storage ps JOIN projects p ON p.id = ps.project_id
WHERE p.org_id = @org_id AND p.deleted_at IS NULL;

-- name: SetStorageLimits :execrows
-- tenant: system - the storage sweep: what the edge enforces, changed only when it differs (a change moves the feed).
UPDATE project_services SET storage_quota_bytes = @quota_bytes, upload_max_bytes = @upload_max_bytes,
  storage_egress_blocked = @egress_blocked, transforms_blocked = @transforms_blocked
WHERE project_id = @project_id AND (storage_quota_bytes IS DISTINCT FROM @quota_bytes OR upload_max_bytes IS DISTINCT FROM @upload_max_bytes
  OR storage_egress_blocked <> @egress_blocked OR transforms_blocked <> @transforms_blocked);

-- name: OrgUsageSince :one
-- tenant: system - the storage sweep: an organisation's use of a metric since a point (this month).
SELECT coalesce(sum(quantity), 0)::numeric FROM usage_records WHERE org_id = @org_id AND metric = @metric AND period_start >= @since;

-- name: GetProjectStorage :one
-- tenant: system - a project the request already authorized: its measured files.
SELECT * FROM project_storage WHERE project_id = @project_id;

-- name: SetStorageReconciled :exec
-- tenant: system - the nightly storage reconciler's findings.
INSERT INTO project_storage (project_id, missing_objects, missing_sample, orphans_removed, transform_bytes, reconciled_at)
VALUES (@project_id, @missing_objects, @missing_sample, @orphans_removed, @transform_bytes, now())
ON CONFLICT (project_id) DO UPDATE SET missing_objects = EXCLUDED.missing_objects, missing_sample = EXCLUDED.missing_sample,
  orphans_removed = project_storage.orphans_removed + EXCLUDED.orphans_removed, transform_bytes = EXCLUDED.transform_bytes, reconciled_at = now();

-- name: ScheduleStorageCleanups :execrows
-- tenant: system - the storage sweep: deleted projects' files go after the final backups' retention (V2 §10.10).
INSERT INTO storage_cleanups (project_id, region, prefix, residency, not_before)
SELECT p.id, p.region, 'files/' || s.ref || '/', p.data_residency, coalesce(p.deleted_at, now()) + make_interval(days => @keep_days::int)
FROM project_services s JOIN projects p ON p.id = s.project_id
WHERE s.schema_version >= 4 AND p.status = 'deleted'
  AND NOT EXISTS (SELECT 1 FROM storage_cleanups c WHERE c.project_id = p.id);

-- name: DueStorageCleanups :many
-- tenant: system - the storage sweep: deleted projects' files due to go.
SELECT * FROM storage_cleanups WHERE not_before <= now() AND attempts < 50 ORDER BY not_before LIMIT 20;

-- name: FinishStorageCleanup :exec
-- tenant: system - the storage sweep: a deleted project's files are gone (the row stays as the record it was done).
UPDATE storage_cleanups SET attempts = attempts + 1, last_error = NULL, not_before = 'infinity' WHERE id = @id;

-- name: FailStorageCleanup :exec
-- tenant: system - the storage sweep: a clean-up to retry later.
UPDATE storage_cleanups SET attempts = attempts + 1, last_error = @last_error, not_before = now() + interval '1 hour' WHERE id = @id;

-- name: RealtimeProjects :many
-- tenant: system - the realtime sweep: running projects with backend services' realtime.
SELECT sqlc.embed(p), s.ref FROM project_services s JOIN projects p ON p.id = s.project_id
WHERE s.enabled AND s.schema_version >= 5 AND p.deleted_at IS NULL AND p.status = 'active' AND p.lifecycle = 'active'
ORDER BY p.org_id, p.id;

-- name: SetRealtimeLimits :execrows
-- tenant: system - the realtime sweep: what the edge enforces, changed only when it differs (a change moves the feed).
UPDATE project_services SET realtime_max_connections = @max_connections, realtime_messages_blocked = @messages_blocked
WHERE project_id = @project_id AND (realtime_max_connections IS DISTINCT FROM @max_connections
  OR realtime_messages_blocked <> @messages_blocked);

-- name: ProjectUsageSince :one
-- tenant: system - a project the request already authorized: its use of a metric since a point (this month).
SELECT coalesce(sum(quantity), 0)::numeric FROM usage_records WHERE project_id = @project_id AND metric = @metric AND period_start >= @since;

-- name: PlanLimitProjects :many
-- tenant: system - the plan-limits sweep (V4.1 §3): every project with backend services on.
SELECT s.project_id, s.plan_timeout_ms, s.plan_rate_per_ip, s.plan_rate_per_key, s.api_requests_blocked, s.mau_blocked, s.mau_counted, p.org_id
FROM project_services s JOIN projects p ON p.id = s.project_id
WHERE s.enabled AND p.deleted_at IS NULL
ORDER BY p.org_id, p.id;

-- name: SetPlanLimits :execrows
-- tenant: system - the plan-limits sweep: what the edge applies, changed only when it differs (a change moves the feed).
UPDATE project_services SET plan_timeout_ms = sqlc.narg(timeout_ms), plan_rate_per_ip = sqlc.narg(rate_per_ip),
  plan_rate_per_key = sqlc.narg(rate_per_key), api_requests_blocked = @api_requests_blocked, mau_blocked = @mau_blocked,
  mau_counted = sqlc.narg(mau_counted)
WHERE project_id = @project_id AND (plan_timeout_ms IS DISTINCT FROM sqlc.narg(timeout_ms)
  OR plan_rate_per_ip IS DISTINCT FROM sqlc.narg(rate_per_ip) OR plan_rate_per_key IS DISTINCT FROM sqlc.narg(rate_per_key)
  OR api_requests_blocked <> @api_requests_blocked OR mau_blocked <> @mau_blocked
  OR mau_counted IS DISTINCT FROM sqlc.narg(mau_counted));

-- name: ProjectMonthUsers :many
-- tenant: system - the plan-limits sweep: a project's users counted as active this month.
SELECT user_id FROM auth_mau WHERE project_id = @project_id AND month = @month;

-- name: InsertPlanLimitNotice :execrows
-- tenant: system - the plan-limits sweep: one notice per organisation, limit, month and level.
INSERT INTO plan_limit_notices (org_id, limit_key, month, level) VALUES (@org_id, @limit_key, @month, @level) ON CONFLICT DO NOTHING;

-- name: CopyServicesSettings :exec
-- tenant: system - a branch takes its parent's API settings (V4.1 §9.5).
UPDATE project_services b SET exposed_schemas = p.exposed_schemas, public_tables = p.public_tables,
  cors_origins = p.cors_origins, settings = p.settings
FROM project_services p WHERE b.project_id = @branch AND p.project_id = @parent;

-- name: CopyAuthConfig :exec
-- tenant: system - a branch takes its parent's auth settings and templates, never its secrets (providers_enc).
INSERT INTO project_auth_config (project_id, config, templates)
SELECT @branch, a.config, a.templates FROM project_auth_config a WHERE a.project_id = @parent
ON CONFLICT (project_id) DO UPDATE SET config = excluded.config, templates = excluded.templates, updated_at = now();
