-- The Free tier's pause and archive (V3 §4).

-- name: TouchProjectsActive :exec
-- tenant: system - the metrics collector marks projects clients used, across organisations.
UPDATE projects SET last_active_at = now(), pause_warned_at = NULL
WHERE id = ANY(@ids::uuid[]) AND deleted_at IS NULL;

-- name: FreeProjectsIdleSince :many
-- tenant: system - the Free tier sweep across organisations: active Free projects idle since before @idle_before.
SELECT p.* FROM projects p
JOIN organizations o ON o.id = p.org_id
LEFT JOIN quota_plans qp ON qp.id = o.plan_id
LEFT JOIN billing_accounts b ON b.org_id = p.org_id
WHERE p.deleted_at IS NULL AND p.status = 'active' AND p.lifecycle = 'active' AND p.tier = 'shared'
  AND o.status = 'active' AND coalesce(b.plan, 'free') = 'free'
  -- The Unlimited quota plan is the platform's own (its admin's organisation): never paused.
  AND coalesce(qp.name, '') <> 'Unlimited'
  AND coalesce(p.last_active_at, p.created_at) < @idle_before
ORDER BY p.created_at;

-- name: MarkPauseWarned :execrows
-- tenant: system - the Free tier sweep claims the 24-hour notice (once).
UPDATE projects SET pause_warned_at = now() WHERE id = @id AND pause_warned_at IS NULL AND lifecycle = 'active';

-- name: SetProjectPaused :one
-- tenant: system - a pause operation on a project it already loaded.
UPDATE projects SET lifecycle = 'paused', paused_at = now() WHERE id = @id AND lifecycle = 'active' RETURNING *;

-- name: SetProjectAwake :one
-- tenant: system - a resume or unarchive operation on a project it already loaded.
UPDATE projects SET lifecycle = 'active', paused_at = NULL, archived_at = NULL, pause_warned_at = NULL,
  archive_notice_days = NULL, last_active_at = now()
WHERE id = @id RETURNING *;

-- name: SetProjectArchived :one
-- tenant: system - an archive operation on a project it already loaded.
UPDATE projects SET lifecycle = 'archived', archived_at = now(), archive_backup_id = @backup_id
WHERE id = @id AND lifecycle = 'paused' RETURNING *;

-- name: FreeProjectsPausedSince :many
-- tenant: system - the Free tier sweep across organisations: Free projects paused before @paused_before.
SELECT p.* FROM projects p
JOIN organizations o ON o.id = p.org_id
LEFT JOIN quota_plans qp ON qp.id = o.plan_id
LEFT JOIN billing_accounts b ON b.org_id = p.org_id
WHERE p.deleted_at IS NULL AND p.status = 'active' AND p.lifecycle = 'paused' AND p.paused_at < @paused_before
  AND o.status = 'active' AND coalesce(b.plan, 'free') = 'free'
  AND coalesce(qp.name, '') <> 'Unlimited'
ORDER BY p.paused_at;

-- name: ProjectSleepPlan :one
-- tenant: system - a pause or archive operation re-checks the plan of the project it already loaded.
-- What decides whether the Free tier may pause or archive a project: its
-- organisation's status, billing plan and quota plan.
SELECT o.status AS org_status, coalesce(b.plan, 'free')::text AS plan, coalesce(qp.name, '')::text AS quota_plan
FROM projects p
JOIN organizations o ON o.id = p.org_id
LEFT JOIN quota_plans qp ON qp.id = o.plan_id
LEFT JOIN billing_accounts b ON b.org_id = p.org_id
WHERE p.id = @id;

-- name: ArchivedProjects :many
-- tenant: system - the Free tier sweep across organisations: every archived project, oldest first.
SELECT p.* FROM projects p
WHERE p.deleted_at IS NULL AND p.lifecycle = 'archived'
ORDER BY p.archived_at;

-- name: SetArchiveNotice :execrows
-- tenant: system - the Free tier sweep claims a deletion notice (once).
UPDATE projects SET archive_notice_days = @days
WHERE id = @id AND lifecycle = 'archived' AND (archive_notice_days IS NULL OR archive_notice_days > @days);

-- name: SleepingPaidProjects :many
-- tenant: system - the Free tier sweep across organisations: paused or archived projects whose organisation now pays, or is on the Unlimited quota plan (which never sleeps).
SELECT p.* FROM projects p
JOIN organizations o ON o.id = p.org_id
LEFT JOIN quota_plans qp ON qp.id = o.plan_id
JOIN billing_accounts b ON b.org_id = p.org_id
WHERE p.deleted_at IS NULL AND p.status = 'active' AND p.lifecycle <> 'active'
  AND (b.plan <> 'free' OR coalesce(qp.name, '') = 'Unlimited');

-- name: ProjectByDatabase :one
-- tenant: system - the waker looks up a client-facing database name across organisations.
SELECT * FROM projects WHERE (db_name = @db OR alias_db_name = @db) AND deleted_at IS NULL
ORDER BY created_at DESC LIMIT 1;

-- name: CountSleepingProjects :one
-- tenant: system - admin console totals of paused and archived projects.
SELECT count(*) FILTER (WHERE lifecycle = 'paused')::bigint AS paused, count(*) FILTER (WHERE lifecycle = 'archived')::bigint AS archived
FROM projects WHERE deleted_at IS NULL;
