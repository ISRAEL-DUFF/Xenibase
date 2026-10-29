-- name: FireAlert :one
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Inserts a firing alert, or refreshes the one already firing for key.
-- inserted tells the caller a notification is due.
INSERT INTO alerts (kind, key, severity, target_type, target_id, target_name, summary, detail)
VALUES (@kind, @key, @severity, @target_type, @target_id, @target_name, @summary, @detail)
ON CONFLICT (key) WHERE status = 'firing'
DO UPDATE SET last_seen_at = now(), summary = EXCLUDED.summary, detail = EXCLUDED.detail, severity = EXCLUDED.severity
RETURNING id, (xmax = 0) AS inserted;

-- name: FiringAlerts :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
SELECT * FROM alerts WHERE status = 'firing' ORDER BY started_at;

-- name: ResolveAlert :execrows
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
UPDATE alerts SET status = 'resolved', resolved_at = now() WHERE id = @id AND status = 'firing';

-- name: ClaimUndelivered :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Alerts with a notification not yet delivered, locked for a minute so
-- only one server sends each.
UPDATE alerts SET delivery_lock = now() + interval '1 minute'
WHERE id IN (
  SELECT id FROM alerts
  WHERE ((status = 'firing' AND notified_at IS NULL) OR (status = 'resolved' AND notified_at IS NOT NULL AND resolved_notified_at IS NULL))
    AND (delivery_lock IS NULL OR delivery_lock < now())
    AND started_at > now() - interval '7 days'
  ORDER BY started_at
  LIMIT 50
  FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: MarkDelivered :exec
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
UPDATE alerts SET
  notified_at = CASE WHEN @resolved::bool THEN notified_at ELSE now() END,
  resolved_notified_at = CASE WHEN @resolved::bool THEN now() ELSE resolved_notified_at END,
  delivery_attempts = delivery_attempts + 1, delivery_error = NULL, delivery_lock = NULL
WHERE id = @id;

-- name: MarkDeliveryFailed :exec
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
UPDATE alerts SET delivery_attempts = delivery_attempts + 1, delivery_error = @error,
  delivery_lock = now() + LEAST(interval '1 minute' * power(2, delivery_attempts), interval '30 minutes')
WHERE id = @id;

-- name: SkipDelivery :exec
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Nothing configured to deliver to: record it as handled.
UPDATE alerts SET
  notified_at = COALESCE(notified_at, now()),
  resolved_notified_at = CASE WHEN status = 'resolved' THEN now() ELSE resolved_notified_at END,
  delivery_lock = NULL
WHERE id = @id;

-- name: ListAlerts :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
SELECT * FROM alerts
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
ORDER BY (status = 'firing') DESC, started_at DESC
LIMIT @max_rows;

-- name: CountFiringAlerts :one
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
SELECT count(*) FILTER (WHERE severity = 'critical') AS critical, count(*) AS total FROM alerts WHERE status = 'firing';

-- Conditions -------------------------------------------------------------------

-- name: FailedLatestOperations :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Kinds whose most recent finished run, per project (or none), failed.
SELECT DISTINCT ON (o.kind, o.project_id, o.params->>'instance_id')
  o.kind, o.project_id, COALESCE(o.params->>'instance_id', '')::text AS instance_id, o.status, o.error, o.finished_at, o.id
FROM operations o
WHERE o.kind = ANY(@kinds::text[]) AND o.status IN ('succeeded', 'failed')
ORDER BY o.kind, o.project_id, o.params->>'instance_id', o.created_at DESC;

-- name: OverdueBackups :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Active projects older than @age whose newest successful backup is older
-- than @age (or missing). Only those the nightly scheduler backs up, as
-- ProjectsDueForBackup picks them: not paused or archived, not a branch
-- without backups, not in a suspended or deleting organisation.
SELECT p.id, p.name, COALESCE((SELECT max(b.finished_at) FROM backups b WHERE b.project_id = p.id AND b.status = 'succeeded'), 'epoch')::timestamptz AS last_backup_at
FROM projects p
WHERE p.deleted_at IS NULL AND p.status = 'active' AND p.created_at < now() - make_interval(secs => @age_seconds::float8)
  AND p.lifecycle = 'active'
  AND (p.parent_project_id IS NULL OR p.branch_backups)
  AND EXISTS (SELECT 1 FROM organizations o WHERE o.id = p.org_id AND o.status = 'active')
  AND NOT EXISTS (SELECT 1 FROM backups b WHERE b.project_id = p.id AND b.status = 'succeeded' AND b.finished_at > now() - make_interval(secs => @age_seconds::float8));

-- name: UnreachableNodes :many
SELECT id, name, last_reachable_at, created_at FROM nodes
WHERE status <> 'removed' AND agent_cert_fp IS NOT NULL
  AND COALESCE(last_reachable_at, created_at) < now() - make_interval(secs => @for_seconds::float8);

-- name: NodeCapacities :many
SELECT id, name, capacity FROM nodes WHERE status <> 'removed' AND agent_cert_fp IS NOT NULL;

-- name: WALArchiveSamples :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Each active dedicated project's WAL archiver samples since @since (-1:
-- none): failed attempts at the oldest and newest sample, and the newest
-- count and age of segments waiting.
SELECT p.id, p.name,
  COALESCE((SELECT m.value FROM metric_points m WHERE m.scope = 'project' AND m.scope_id = p.id AND m.metric = 'wal_archive_failed'
     AND m.resolution = '1m' AND m.ts > @since ORDER BY m.ts LIMIT 1), -1)::float8 AS failed_first,
  COALESCE((SELECT m.value FROM metric_points m WHERE m.scope = 'project' AND m.scope_id = p.id AND m.metric = 'wal_archive_failed'
     AND m.resolution = '1m' AND m.ts > @since ORDER BY m.ts DESC LIMIT 1), -1)::float8 AS failed_last,
  COALESCE((SELECT m.value FROM metric_points m WHERE m.scope = 'project' AND m.scope_id = p.id AND m.metric = 'wal_archive_pending'
     AND m.resolution = '1m' AND m.ts > @since ORDER BY m.ts DESC LIMIT 1), -1)::float8 AS pending,
  COALESCE((SELECT m.value FROM metric_points m WHERE m.scope = 'project' AND m.scope_id = p.id AND m.metric = 'wal_archive_pending_age_s'
     AND m.resolution = '1m' AND m.ts > @since ORDER BY m.ts DESC LIMIT 1), -1)::float8 AS pending_age_s
FROM projects p WHERE p.deleted_at IS NULL AND p.status = 'active' AND p.tier = 'dedicated';

-- name: ProjectSizes :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Each active project's latest size sample (within @since) and settings.
SELECT p.id, p.name, p.settings,
  COALESCE((SELECT m.value FROM metric_points m WHERE m.scope = 'project' AND m.scope_id = p.id AND m.metric = 'size_bytes'
     AND m.resolution = '1m' AND m.ts > @since ORDER BY m.ts DESC LIMIT 1), -1)::float8 AS size_bytes
FROM projects p WHERE p.deleted_at IS NULL AND p.status = 'active';
