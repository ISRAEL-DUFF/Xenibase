-- name: FireAlert :one
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Inserts a firing alert, or refreshes the one already firing for key
-- (and forgets the clear evaluations it had: hysteresis counts them in a
-- row). inserted tells the caller a notification is due.
INSERT INTO alerts (kind, key, severity, target_type, target_id, target_name, summary, detail)
VALUES (@kind, @key, @severity, @target_type, @target_id, @target_name, @summary, @detail)
ON CONFLICT (key) WHERE status = 'firing'
DO UPDATE SET last_seen_at = now(), summary = EXCLUDED.summary, detail = EXCLUDED.detail, severity = EXCLUDED.severity,
  clear_count = 0, cleared_since = NULL
RETURNING id, (xmax = 0) AS inserted;

-- name: FiringAlerts :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
SELECT * FROM alerts WHERE status = 'firing' ORDER BY started_at;

-- name: ClearAlert :one
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- One evaluation found a firing alert's condition clear: count it, and
-- resolve the alert once @resolve_after evaluations in a row did, the
-- first at least @span_seconds ago (several servers evaluate, so the
-- count alone would come sooner). resolved tells the caller it resolved.
UPDATE alerts SET
  clear_count = clear_count + 1,
  cleared_since = COALESCE(cleared_since, now()),
  status = CASE WHEN clear_count + 1 >= @resolve_after::int AND COALESCE(cleared_since, now()) <= now() - make_interval(secs => @span_seconds::float8)
    THEN 'resolved' ELSE status END,
  resolved_at = CASE WHEN clear_count + 1 >= @resolve_after::int AND COALESCE(cleared_since, now()) <= now() - make_interval(secs => @span_seconds::float8)
    THEN now() ELSE resolved_at END
WHERE id = @id AND status = 'firing'
RETURNING (status = 'resolved')::bool AS resolved;

-- name: PruneResolvedAlerts :execrows
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Resolved alerts (and, by cascade, their deliveries) older than @before.
DELETE FROM alerts WHERE status = 'resolved' AND resolved_at < @before::timestamptz;

-- name: ClaimUndelivered :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Alerts with a notification not yet delivered on every channel, locked
-- for a minute so only one server sends each. A resolved alert is
-- claimed whether or not its firing was delivered: one that resolved
-- before that is announced by its resolution. Each event is retried for
-- 7 days from when it happened.
UPDATE alerts SET delivery_lock = now() + interval '1 minute'
WHERE id IN (
  SELECT id FROM alerts
  WHERE ((status = 'firing' AND notified_at IS NULL AND started_at > now() - interval '7 days')
      OR (status = 'resolved' AND resolved_notified_at IS NULL AND resolved_at > now() - interval '7 days'))
    AND (delivery_lock IS NULL OR delivery_lock < now())
  ORDER BY started_at
  LIMIT 50
  FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: AlertDeliveries :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Each channel's record of one event of an alert.
SELECT * FROM alert_deliveries WHERE alert_id = @alert_id AND event = @event;

-- name: MarkChannelSent :exec
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
INSERT INTO alert_deliveries (alert_id, event, channel, sent_at, attempts)
VALUES (@alert_id, @event, @channel, now(), 1)
ON CONFLICT (alert_id, event, channel) DO UPDATE SET sent_at = now(), attempts = alert_deliveries.attempts + 1,
  error = NULL, next_at = NULL;

-- name: MarkChannelFailed :one
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- A channel failed: it is retried alone after 1 minute × 2^attempts,
-- capped at 30 minutes. Returns when.
INSERT INTO alert_deliveries (alert_id, event, channel, attempts, error, next_at)
VALUES (@alert_id, @event, @channel, 1, @error, now() + interval '1 minute')
ON CONFLICT (alert_id, event, channel) DO UPDATE SET attempts = alert_deliveries.attempts + 1, error = EXCLUDED.error,
  next_at = now() + LEAST(interval '1 minute' * power(2, alert_deliveries.attempts), interval '30 minutes')
RETURNING next_at::timestamptz;

-- name: MarkDelivered :exec
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Every configured channel has the event.
UPDATE alerts SET
  notified_at = CASE WHEN @resolved::bool THEN notified_at ELSE now() END,
  resolved_notified_at = CASE WHEN @resolved::bool THEN now() ELSE resolved_notified_at END,
  delivery_attempts = delivery_attempts + 1, delivery_error = NULL, delivery_lock = NULL
WHERE id = @id;

-- name: MarkDeliveryFailed :exec
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Some channel still lacks the event: claim the alert again when the
-- earliest such channel's backoff ends.
UPDATE alerts SET delivery_attempts = delivery_attempts + 1, delivery_error = @error, delivery_lock = @retry_at
WHERE id = @id;

-- name: SkipDelivery :exec
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Nothing configured to deliver to: record it as handled.
UPDATE alerts SET
  notified_at = COALESCE(notified_at, now()),
  resolved_notified_at = CASE WHEN status = 'resolved' THEN now() ELSE resolved_notified_at END,
  delivery_lock = NULL
WHERE id = @id;

-- name: GraceFailing :one
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- key is failing now: returns for how many seconds, by the database's
-- clock and shared by every server. A row no server refreshed for
-- @stale_seconds starts over.
INSERT INTO alert_grace (key) VALUES (@key)
ON CONFLICT (key) DO UPDATE SET
  since = CASE WHEN alert_grace.seen_at < now() - make_interval(secs => @stale_seconds::float8) THEN now() ELSE alert_grace.since END,
  seen_at = now()
-- clock_timestamp, not now(): a key first seen in this statement has been
-- failing for a moment, not for no time, so a grace period of next to
-- nothing is already over.
RETURNING EXTRACT(EPOCH FROM clock_timestamp() - since)::float8 AS failing_seconds;

-- name: GraceClear :exec
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
DELETE FROM alert_grace WHERE key = ANY(@keys::text[]);

-- name: GraceClearPrefix :exec
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Forgets keys with @prefix except @keep.
DELETE FROM alert_grace WHERE starts_with(key, @prefix::text) AND NOT (key = ANY(@keep::text[]));

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

-- name: FailedMinorUpgrades :many
-- tenant: system - the platform alert evaluator; alerts are the platform admin's.
-- Live instances whose latest finished minor upgrade failed.
SELECT l.id, l.instance_id, l.from_release, l.to_release, COALESCE(l.error, '')::text AS error, l.finished_at, i.kind, n.name AS node_name
FROM (
  SELECT DISTINCT ON (m.instance_id) m.* FROM minor_upgrades m
  WHERE m.finished_at IS NOT NULL
  ORDER BY m.instance_id, m.started_at DESC
) l
JOIN instances i ON i.id = l.instance_id AND i.deleted_at IS NULL
JOIN nodes n ON n.id = i.node_id
WHERE l.error IS NOT NULL;

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
