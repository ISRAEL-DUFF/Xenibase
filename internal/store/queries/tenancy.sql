-- Tenancy hardening, quotas, and usage (V2 s10). Background workers here
-- look across organisations by design; each says so.

-- name: SharedProjectSizes :many
-- size_bytes is -1 for a project not measured in the last 30 minutes.
-- tenant: system - storage enforcement checks every shared-tier project.
SELECT p.id, p.org_id, p.name, p.db_name, p.instance_id, p.storage_state,
       COALESCE((SELECT m.value FROM metric_points m
        WHERE m.scope = 'project' AND m.scope_id = p.id AND m.metric = 'size_bytes' AND m.resolution = '1m'
          AND m.ts > now() - interval '30 minutes'
        ORDER BY m.ts DESC LIMIT 1), -1)::float8 AS size_bytes,
       COALESCE((SELECT u.value / NULLIF(t.value, 0) FROM metric_points u JOIN metric_points t
          ON t.scope = 'node' AND t.scope_id = u.scope_id AND t.metric = 'disk_total_bytes' AND t.resolution = '1m' AND t.ts = u.ts
        WHERE u.scope = 'node' AND u.scope_id = i.node_id AND u.metric = 'disk_used_bytes' AND u.resolution = '1m'
          AND u.ts > now() - interval '30 minutes'
        ORDER BY u.ts DESC LIMIT 1), 0)::float8 AS node_disk_used
FROM projects p JOIN instances i ON i.id = p.instance_id
WHERE p.deleted_at IS NULL AND p.tier = 'shared' AND p.status = 'active';

-- name: SetProjectStorageState :exec
-- tenant: system - storage enforcement on a project it just measured.
UPDATE projects SET storage_state = @storage_state, storage_state_at = now() WHERE id = @id;

-- name: OrgOwnerEmails :many
SELECT u.email FROM org_members m JOIN users u ON u.id = m.user_id
WHERE m.org_id = @org_id AND m.role = 'owner' AND u.disabled_at IS NULL ORDER BY u.email;

-- name: ProjectAdminEmails :many
-- Org owners and admins, and the project's own admins.
SELECT DISTINCT u.email FROM users u
WHERE u.disabled_at IS NULL AND (
  EXISTS (SELECT 1 FROM org_members m WHERE m.user_id = u.id AND m.org_id = @org_id AND m.role IN ('owner', 'admin'))
  OR EXISTS (SELECT 1 FROM project_members pm WHERE pm.user_id = u.id AND pm.org_id = @org_id
             AND pm.project_id = @project_id AND pm.role = 'admin'))
ORDER BY u.email;

-- name: CountOrgProjects :one
-- Projects that count towards the projects quota (branches don't, V2 s10.3).
SELECT count(*)::int FROM projects WHERE org_id = @org_id AND deleted_at IS NULL AND parent_project_id IS NULL;

-- name: CountOrgOperationsInFlight :one
-- Backups, restores, imports, and other work queued or running for the org.
SELECT count(*)::int FROM operations o JOIN projects p ON p.id = o.project_id
WHERE p.org_id = @org_id AND o.status IN ('queued', 'running')
  AND o.kind = ANY(@kinds::text[]);

-- name: OrgSharedStorage :one
-- tenant: system - the org's latest measured shared-tier size, joined by org_id.
SELECT COALESCE(sum(s.v), 0)::float8 FROM (
  SELECT (SELECT m.value FROM metric_points m
          WHERE m.scope = 'project' AND m.scope_id = p.id AND m.metric = 'size_bytes' AND m.resolution = '1m'
          ORDER BY m.ts DESC LIMIT 1) AS v
  FROM projects p WHERE p.org_id = @org_id AND p.deleted_at IS NULL AND p.tier = 'shared') s;

-- name: InsertReapedSession :exec
INSERT INTO reaped_sessions (project_id, org_id, kind, role_name, duration_s, query, pid, started_at)
VALUES (@project_id, @org_id, @kind, @role_name, @duration_s, sqlc.narg(query), sqlc.narg(pid), sqlc.narg(started_at));

-- name: ReapedStatementCancelled :one
-- Whether the reaper already cancelled this backend's statement (the same
-- pid and query_start), so a statement still running caught the cancel.
SELECT EXISTS (SELECT 1 FROM reaped_sessions
  WHERE project_id = @project_id AND org_id = @org_id AND kind = 'statement'
    AND pid = @pid AND started_at = @started_at)::bool;

-- name: ListReapedSessions :many
SELECT * FROM reaped_sessions WHERE project_id = @project_id AND org_id = @org_id
ORDER BY created_at DESC, id DESC LIMIT @max_rows;

-- name: SharedInstanceProjects :many
-- tenant: system - the reaper looks at every tenant database on a shared cluster.
SELECT p.id, p.org_id, p.db_name, p.instance_id FROM projects p
JOIN instances i ON i.id = p.instance_id
WHERE p.deleted_at IS NULL AND p.tier = 'shared' AND i.kind = 'shared';

-- name: HourlySharedStorage :many
-- tenant: system - usage recording measures every project; rows carry org_id.
-- Average measured size per project per hour in [from, to), from 1-minute
-- points where they still exist and hourly points otherwise.
WITH pts AS (
  SELECT m.scope_id, date_trunc('hour', m.ts) AS h, m.value, m.resolution
  FROM metric_points m
  WHERE m.scope = 'project' AND m.metric = 'size_bytes' AND m.ts >= @from_ts::timestamptz AND m.ts < @to_ts::timestamptz
), minute AS (
  SELECT scope_id, h, avg(value) AS v FROM pts WHERE resolution = '1m' GROUP BY scope_id, h
), hour AS (
  SELECT scope_id, h, avg(value) AS v FROM pts WHERE resolution = '1h' GROUP BY scope_id, h
)
SELECT p.id AS project_id, p.org_id, o.plan_id, x.h::timestamptz AS period_start, x.v::float8 AS avg_bytes,
       (p.parent_project_id IS NOT NULL)::bool AS is_branch
FROM (SELECT * FROM minute
      UNION ALL
      SELECT * FROM hour WHERE NOT EXISTS (SELECT 1 FROM minute WHERE minute.scope_id = hour.scope_id AND minute.h = hour.h)) x
JOIN projects p ON p.id = x.scope_id
JOIN organizations o ON o.id = p.org_id
WHERE p.tier = 'shared';

-- name: HourlyDedicated :many
-- tenant: system - usage recording; rows carry org_id.
-- Each dedicated project's instance size, for each hour from @from_ts to
-- @last_hour it existed, with the fraction of the hour it did.
SELECT p.id AS project_id, p.org_id, o.plan_id, g.h::timestamptz AS period_start,
       COALESCE(i.cpu_limit, 0)::float8 AS cpus, COALESCE(i.mem_limit_mb, 0)::int AS mem_mb, COALESCE(i.volume_gb, 0)::int AS disk_gb,
       -- HA standbys (V3 §2.2): the members besides the primary, each the instance's size
       -- (read replicas are recorded on their own, V4 §7).
       (CASE WHEN i.ha_enabled THEN GREATEST((SELECT count(*) FROM instance_members m WHERE m.instance_id = i.id AND m.deleted_at IS NULL AND NOT m.replica) - 1, 0) ELSE 0 END)::int AS standbys,
       (i.ha_enabled AND i.sync_replication)::bool AS sync_replication,
       i.pitr_days,
       (extract(epoch FROM LEAST(g.h + '1 hour'::interval, COALESCE(p.deleted_at, 'infinity'::timestamptz)) - GREATEST(g.h, p.created_at)) / 3600)::float8 AS fraction
FROM projects p
JOIN instances i ON i.id = p.instance_id
JOIN organizations o ON o.id = p.org_id
CROSS JOIN generate_series(@from_ts::timestamptz, @last_hour::timestamptz, '1 hour'::interval) AS g(h)
WHERE p.tier = 'dedicated' AND i.kind = 'dedicated'
  AND p.created_at < g.h + '1 hour'::interval AND (p.deleted_at IS NULL OR p.deleted_at > g.h);

-- name: HourlyBackupRetention :many
-- tenant: system - usage recording; rows carry org_id.
-- Projects on a longer backup retention (V4.1 §4.2), for each hour from
-- @from_ts to @last_hour they existed, with the fraction of the hour.
SELECT p.id AS project_id, p.org_id, o.plan_id, g.h::timestamptz AS period_start, (p.settings->>'backup_retention')::text AS retention,
       (extract(epoch FROM LEAST(g.h + '1 hour'::interval, COALESCE(p.deleted_at, 'infinity'::timestamptz)) - GREATEST(g.h, p.created_at)) / 3600)::float8 AS fraction
FROM projects p
JOIN organizations o ON o.id = p.org_id
CROSS JOIN generate_series(@from_ts::timestamptz, @last_hour::timestamptz, '1 hour'::interval) AS g(h)
WHERE p.settings->>'backup_retention' IN ('extended', 'long')
  AND p.created_at < g.h + '1 hour'::interval AND (p.deleted_at IS NULL OR p.deleted_at > g.h);

-- name: DailyBackupBytes :many
-- tenant: system - usage recording; rows carry org_id.
-- Bytes of successful backups each project held during [day_start, day_end)
-- on platform targets: storage on an org's own target is the org's bill
-- (V2 s6).
SELECT p.id AS project_id, p.org_id, o.plan_id, COALESCE(sum(b.size_bytes), 0)::float8 AS bytes
FROM backups b JOIN projects p ON p.id = b.project_id JOIN organizations o ON o.id = p.org_id
WHERE b.status IN ('succeeded', 'deleted', 'copied') AND b.size_bytes IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM storage_targets t WHERE t.id = b.storage_target_id AND t.org_id IS NOT NULL)
  AND b.finished_at < @day_end::timestamptz
  AND (b.deleted_at IS NULL OR b.deleted_at > @day_start::timestamptz)
GROUP BY p.id, p.org_id, o.plan_id;

-- name: UpsertUsage :exec
INSERT INTO usage_records (org_id, project_id, metric, granularity, period_start, quantity, plan_id)
VALUES (@org_id, @project_id, @metric, @granularity, @period_start, @quantity, @plan_id)
ON CONFLICT (org_id, metric, granularity, period_start, project_id)
DO UPDATE SET quantity = EXCLUDED.quantity;

-- name: AddUsage :exec
-- Adds to a counter metric's period (pooler transfer).
INSERT INTO usage_records (org_id, project_id, metric, granularity, period_start, quantity, plan_id)
VALUES (@org_id, @project_id, @metric, 'hour', @period_start, @quantity, @plan_id)
ON CONFLICT (org_id, metric, granularity, period_start, project_id)
DO UPDATE SET quantity = usage_records.quantity + EXCLUDED.quantity;

-- name: RollupUsage :execrows
-- tenant: system - rolls every org's hourly rows older than @before into daily ones.
WITH old AS (
  DELETE FROM usage_records WHERE granularity = 'hour' AND period_start < @before::timestamptz
  RETURNING org_id, project_id, metric, period_start, quantity, plan_id
)
INSERT INTO usage_records (org_id, project_id, metric, granularity, period_start, quantity, plan_id)
SELECT org_id, project_id, metric, 'day', date_trunc('day', period_start), sum(quantity), (array_agg(plan_id ORDER BY period_start DESC))[1]
FROM old GROUP BY org_id, project_id, metric, date_trunc('day', period_start)
ON CONFLICT (org_id, metric, granularity, period_start, project_id)
DO UPDATE SET quantity = usage_records.quantity + EXCLUDED.quantity;

-- name: OrgUsage :many
SELECT u.metric, u.granularity, u.period_start, u.project_id, u.quantity::float8 AS quantity
FROM usage_records u
WHERE u.org_id = @org_id AND u.period_start >= @from_ts::timestamptz AND u.period_start < @to_ts::timestamptz
  AND (sqlc.narg(metric)::text IS NULL OR u.metric = sqlc.narg(metric))
ORDER BY u.period_start, u.metric, u.project_id;

-- name: PlatformUsage :many
-- tenant: system - the platform admin's per-org totals (no tenant content).
SELECT u.org_id, o.name AS org_name, u.metric, sum(u.quantity)::float8 AS quantity
FROM usage_records u JOIN organizations o ON o.id = u.org_id
WHERE u.period_start >= @from_ts::timestamptz AND u.period_start < @to_ts::timestamptz
GROUP BY u.org_id, o.name, u.metric
ORDER BY o.name, u.metric;

-- name: ProjectPoolerNames :many
-- tenant: system - pooler transfer is reported per database name; map it back.
SELECT p.id AS project_id, p.org_id, o.plan_id, p.db_name, p.alias_db_name
FROM projects p JOIN organizations o ON o.id = p.org_id WHERE p.deleted_at IS NULL;

-- name: OrgWithPlan :one
SELECT o.*, q.name AS plan_name, q.limits AS plan_limits
FROM organizations o JOIN quota_plans q ON q.id = o.plan_id
WHERE o.id = @org_id;


-- name: ActiveBreakGlass :one
SELECT * FROM break_glass_sessions
WHERE org_id = @org_id AND admin_id = @admin_id AND ended_at IS NULL AND expires_at > now()
ORDER BY starts_at DESC LIMIT 1;

-- name: OrgActiveBreakGlass :many
SELECT b.*, u.email AS admin_email FROM break_glass_sessions b JOIN users u ON u.id = b.admin_id
WHERE b.org_id = @org_id AND b.ended_at IS NULL AND b.expires_at > now()
ORDER BY b.starts_at DESC;

-- name: InsertBreakGlass :one
INSERT INTO break_glass_sessions (org_id, admin_id, reason, expires_at)
VALUES (@org_id, @admin_id, @reason, @expires_at)
RETURNING *;

-- name: EndBreakGlass :execrows
UPDATE break_glass_sessions SET ended_at = now(), ended_by = sqlc.narg(ended_by)
WHERE id = @id AND org_id = @org_id AND ended_at IS NULL;

-- name: EndExpiredBreakGlass :many
-- tenant: system - closes every org's expired sessions so they read as ended.
UPDATE break_glass_sessions SET ended_at = expires_at
WHERE ended_at IS NULL AND expires_at <= now()
RETURNING *;

-- name: OrgLiveProjects :many
SELECT * FROM projects WHERE org_id = @org_id AND deleted_at IS NULL ORDER BY created_at;

-- name: SuspendOrg :execrows
UPDATE organizations SET status = 'suspended', suspended_reason = @reason, suspended_at = now()
WHERE id = @org_id AND status = 'active';

-- name: ReinstateOrg :execrows
UPDATE organizations SET status = 'active', suspended_reason = NULL, suspended_at = NULL,
  delete_after = NULL, delete_requested_by = NULL
WHERE id = @org_id AND status IN ('suspended', 'deleting');

-- name: MarkOrgDeleting :execrows
UPDATE organizations SET status = 'deleting', delete_after = @delete_after, delete_requested_by = sqlc.narg(requested_by)
WHERE id = @org_id AND status = 'active';

-- name: OrgsDueForDeletion :many
-- tenant: system - the sweep finishes every organisation whose grace period ended.
SELECT * FROM organizations WHERE status = 'deleting' AND delete_after <= @now::timestamptz;

-- name: MarkOrgDeleted :exec
UPDATE organizations SET status = 'deleted' WHERE id = @org_id;

-- name: DeleteOrgMemberships :exec
DELETE FROM org_members WHERE org_id = @org_id;

-- name: RevokeOrgInvitations :exec
UPDATE invitations SET revoked_at = now() WHERE org_id = @org_id AND accepted_at IS NULL AND revoked_at IS NULL;

-- name: ExpireOrgFinalBackups :exec
-- Final backups of a deleted organisation's projects are kept 30 days (V2
-- s10.10), and their nightly and archive backups on platform targets go
-- with them; those on the organisation's own targets are left alone.
UPDATE backups b SET expires_at = @expires_at
FROM projects p WHERE p.id = b.project_id AND p.org_id = @org_id AND b.status IN ('succeeded', 'copied')
  AND (b.kind = 'final' OR (b.kind IN ('logical', 'archive')
    AND (b.storage_target_id IS NULL OR EXISTS (SELECT 1 FROM storage_targets t WHERE t.id = b.storage_target_id AND t.org_id IS NULL))));

-- name: OrgDedicatedUse :one
-- The org's live dedicated instances and their total size: its dedicated
-- projects' instances, and those a demotion keeps stopped for 48 hours (the
-- allowance is released once they are destroyed, V2 s5.2).
SELECT count(*)::int AS instances, COALESCE(sum(i.cpu_limit), 0)::float8 AS cpus,
       COALESCE(sum(i.mem_limit_mb), 0)::int AS mem_mb, COALESCE(sum(i.volume_gb), 0)::int AS disk_gb
FROM instances i
WHERE i.kind = 'dedicated' AND i.deleted_at IS NULL AND (
  EXISTS (SELECT 1 FROM projects p WHERE p.instance_id = i.id AND p.org_id = @org_id AND p.deleted_at IS NULL AND p.tier = 'dedicated')
  OR EXISTS (SELECT 1 FROM retired_databases r JOIN projects p ON p.id = r.project_id
             WHERE r.instance_id = i.id AND r.dropped_at IS NULL AND r.reason = 'demotion' AND p.org_id = @org_id));

-- name: OrgLargestProject :one
-- tenant: system - the org's largest measured shared project, joined by org_id.
SELECT COALESCE(max(s.v), 0)::float8 FROM (
  SELECT (SELECT m.value FROM metric_points m
          WHERE m.scope = 'project' AND m.scope_id = p.id AND m.metric = 'size_bytes' AND m.resolution = '1m'
          ORDER BY m.ts DESC LIMIT 1) AS v
  FROM projects p WHERE p.org_id = @org_id AND p.deleted_at IS NULL AND p.tier = 'shared') s;

-- name: InsertDedicatedRequest :one
INSERT INTO dedicated_requests (org_id, project_id, requested_by, profile, reason)
VALUES (@org_id, @project_id, @requested_by, @profile, sqlc.narg(reason))
RETURNING *;

-- name: GetDedicatedRequest :one
-- tenant: system - the platform admin decides any organisation's request.
SELECT * FROM dedicated_requests WHERE id = @id;

-- name: ListDedicatedRequests :many
-- tenant: system - the platform admin's queue across organisations (no tenant content).
SELECT r.*, o.name AS org_name, p.name AS project_name, u.email AS requested_by_email
FROM dedicated_requests r
JOIN organizations o ON o.id = r.org_id JOIN projects p ON p.id = r.project_id JOIN users u ON u.id = r.requested_by
WHERE (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status))
ORDER BY r.created_at DESC LIMIT 200;

-- name: OrgDedicatedRequests :many
SELECT r.*, p.name AS project_name, u.email AS requested_by_email
FROM dedicated_requests r JOIN projects p ON p.id = r.project_id JOIN users u ON u.id = r.requested_by
WHERE r.org_id = @org_id
ORDER BY r.created_at DESC LIMIT 100;

-- name: DecideDedicatedRequest :execrows
-- tenant: system - the platform admin decides any organisation's request.
UPDATE dedicated_requests SET status = @status, decided_by = @decided_by, decided_at = now(), decision_note = sqlc.narg(note)
WHERE id = @id AND status = 'pending';

-- name: SetOrgDedicatedAllowance :exec
UPDATE organizations SET dedicated_allowance = @allowance WHERE id = @org_id;

-- name: PlatformAdminEmails :many
SELECT email FROM users WHERE platform_role = 'platform_admin' AND disabled_at IS NULL ORDER BY email;

-- name: AdminListOrgs :many
-- tenant: system - the platform admin's organisation list: names, counts, sizes, statuses only.
SELECT o.id, o.name, o.slug, o.status, o.suspended_reason, o.outbound_disabled, o.created_at, o.personal_owner_id,
       q.name AS plan_name,
       (SELECT count(*) FROM org_members m WHERE m.org_id = o.id)::int AS member_count,
       (SELECT count(*) FROM projects p WHERE p.org_id = o.id AND p.deleted_at IS NULL)::int AS project_count,
       COALESCE((SELECT sum((SELECT m.value FROM metric_points m WHERE m.scope = 'project' AND m.scope_id = p.id
                  AND m.metric = 'size_bytes' AND m.resolution = '1m' ORDER BY m.ts DESC LIMIT 1))
                 FROM projects p WHERE p.org_id = o.id AND p.deleted_at IS NULL), 0)::float8 AS size_bytes,
       (SELECT count(*) FROM projects p JOIN storage_targets t ON t.id = p.storage_target_id
        WHERE p.org_id = o.id AND p.deleted_at IS NULL AND t.org_id IS NOT NULL)::int AS org_target_projects
FROM organizations o JOIN quota_plans q ON q.id = o.plan_id
WHERE o.status <> 'deleted'
  AND (sqlc.narg(search)::text IS NULL OR o.name ILIKE '%' || sqlc.narg(search) || '%' OR o.slug ILIKE '%' || sqlc.narg(search) || '%')
ORDER BY o.created_at DESC LIMIT 500;

-- name: AdminUpdateOrg :exec
UPDATE organizations SET plan_id = @plan_id, limit_overrides = @limit_overrides,
  dedicated_allowance = @dedicated_allowance, outbound_disabled = @outbound_disabled
WHERE id = @org_id;

-- name: ListPlans :many
SELECT * FROM quota_plans ORDER BY created_at, name;

-- name: GetPlan :one
SELECT * FROM quota_plans WHERE id = @id;

-- name: InsertPlan :one
INSERT INTO quota_plans (name, limits) VALUES (@name, @limits) RETURNING *;

-- name: UpdatePlan :one
UPDATE quota_plans SET name = @name, limits = @limits WHERE id = @id RETURNING *;

-- name: ListOrgProjectNames :many
-- Every project the org has had, deleted ones included (usage keeps their ids).
SELECT id, name FROM projects WHERE org_id = @org_id;

-- name: CountInstanceProjects :one
-- tenant: system - how full a shared cluster is.
SELECT count(*)::int FROM projects WHERE instance_id = @instance_id AND deleted_at IS NULL;

-- name: ListSharedClusters :many
-- tenant: system - the platform admin's view of shared clusters and their reservations.
SELECT i.id, n.name AS node_name, i.org_id, o.name AS org_name,
       (SELECT count(*) FROM projects p WHERE p.instance_id = i.id AND p.deleted_at IS NULL)::int AS project_count
FROM instances i JOIN nodes n ON n.id = i.node_id LEFT JOIN organizations o ON o.id = i.org_id
WHERE i.kind = 'shared' AND i.deleted_at IS NULL
ORDER BY n.name, i.created_at;
