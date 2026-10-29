-- V2 s9 webhooks and scheduled jobs, s10.7 outbound controls.

-- name: OutboundHostAllowed :one
SELECT EXISTS (SELECT 1 FROM outbound_allowlist WHERE org_id = @org_id AND host = @host)::bool;

-- name: ListOutboundAllowlist :many
SELECT * FROM outbound_allowlist WHERE org_id = @org_id ORDER BY host;

-- name: AddOutboundAllow :exec
INSERT INTO outbound_allowlist (org_id, host, created_by) VALUES (@org_id, @host, sqlc.narg(created_by))
ON CONFLICT DO NOTHING;

-- name: RemoveOutboundAllow :execrows
DELETE FROM outbound_allowlist WHERE org_id = @org_id AND host = @host;

-- name: CountOutbound :exec
INSERT INTO outbound_counters (org_id, host, day, requests, failures) VALUES (@org_id, @host, @day::date, @requests, @failures)
ON CONFLICT (org_id, host, day) DO UPDATE
SET requests = outbound_counters.requests + EXCLUDED.requests, failures = outbound_counters.failures + EXCLUDED.failures;

-- name: ListOutboundCounters :many
-- The organisation's outbound requests per destination host since @since.
SELECT host, sum(requests)::bigint AS requests, sum(failures)::bigint AS failures, max(day)::date AS last_day
FROM outbound_counters WHERE org_id = @org_id AND day >= @since::date
GROUP BY host ORDER BY requests DESC, host LIMIT 200;

-- name: SweepOutboundCounters :execrows
-- tenant: system - retention across organisations.
DELETE FROM outbound_counters WHERE day < @before::date;

-- ---- Webhooks --------------------------------------------------------------

-- name: InsertWebhook :one
-- tenant: system - a project the request already authorized.
INSERT INTO webhooks (id, project_id, name, tables, events, columns, url, headers_enc, secret_enc, enabled, status, created_by, description, metadata)
VALUES (@id, @project_id, @name, @tables, @events, sqlc.narg(columns), @url, sqlc.narg(headers_enc), @secret_enc, @enabled, @status, sqlc.narg(created_by),
  @description, @metadata)
RETURNING *;

-- name: GetWebhook :one
-- tenant: system - a project the request already authorized.
SELECT * FROM webhooks WHERE id = @id AND project_id = @project_id;

-- name: GetWebhookByID :one
-- tenant: system - delivery workers.
SELECT * FROM webhooks WHERE id = @id;

-- name: ListWebhooks :many
-- tenant: system - a project the request already authorized, or delivery workers.
SELECT * FROM webhooks WHERE project_id = @project_id ORDER BY name;

-- name: UpdateWebhook :one
-- tenant: system - a project the request already authorized.
UPDATE webhooks SET name = @name, tables = @tables, events = @events, columns = sqlc.narg(columns), url = @url,
  headers_enc = sqlc.narg(headers_enc), enabled = @enabled, status = @status, status_reason = sqlc.narg(status_reason),
  consecutive_failures = @consecutive_failures, description = @description, metadata = @metadata, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SetWebhookSecret :exec
-- tenant: system - a project the request already authorized.
UPDATE webhooks SET secret_enc = @secret_enc, previous_secret_enc = sqlc.narg(previous_secret_enc),
  previous_secret_expires_at = sqlc.narg(previous_secret_expires_at), updated_at = now()
WHERE id = @id;

-- name: SetWebhookHealth :exec
-- tenant: system - delivery workers.
UPDATE webhooks SET status = @status, status_reason = sqlc.narg(status_reason), consecutive_failures = @consecutive_failures,
  enabled = @enabled, updated_at = now()
WHERE id = @id;

-- name: DeleteWebhook :exec
-- tenant: system - a project the request already authorized.
DELETE FROM webhooks WHERE id = @id;

-- name: DeleteProjectWebhooks :exec
-- tenant: system - project deletion.
DELETE FROM webhooks WHERE project_id = @project_id;

-- name: DeleteProjectJobs :exec
-- tenant: system - project deletion.
DELETE FROM scheduled_jobs WHERE project_id = @project_id;

-- name: WebhookProjects :many
-- tenant: system - delivery workers across organisations.
-- Live projects with at least one webhook.
SELECT p.* FROM projects p
WHERE p.deleted_at IS NULL AND p.lifecycle = 'active' -- paused projects' deliveries wait (V3 §4.2)
  AND EXISTS (SELECT 1 FROM webhooks w WHERE w.project_id = p.id)
ORDER BY p.id;

-- name: InsertDelivery :one
-- tenant: system - delivery workers.
INSERT INTO webhook_deliveries (webhook_id, event_id, attempt, status_code, latency_ms, response_snippet, error, succeeded, dead_lettered, payload, created_at)
VALUES (@webhook_id, @event_id, @attempt, sqlc.narg(status_code), sqlc.narg(latency_ms), sqlc.narg(response_snippet), sqlc.narg(error),
  @succeeded, @dead_lettered, sqlc.narg(payload), @created_at)
RETURNING id;

-- name: ListDeliveries :many
-- tenant: system - a webhook the request already authorized.
SELECT id, webhook_id, event_id, attempt, status_code, latency_ms, response_snippet, error, succeeded, dead_lettered, replayed_at, created_at
FROM webhook_deliveries
WHERE webhook_id = @webhook_id AND (NOT @dead_only::bool OR (dead_lettered AND replayed_at IS NULL))
ORDER BY created_at DESC, id DESC LIMIT @max_rows;

-- name: DeadLetters :many
-- tenant: system - a webhook the request already authorized.
-- Dead letters not yet replayed: all of them, or those in @ids.
SELECT * FROM webhook_deliveries
WHERE webhook_id = @webhook_id AND dead_lettered AND replayed_at IS NULL AND payload IS NOT NULL
  AND (sqlc.narg(ids)::bigint[] IS NULL OR id = ANY(sqlc.narg(ids)::bigint[]))
ORDER BY id;

-- name: MarkReplayed :exec
-- tenant: system - a webhook the request already authorized.
UPDATE webhook_deliveries SET replayed_at = now() WHERE id = ANY(@ids::bigint[]);

-- name: SweepDeliveries :execrows
-- tenant: system - retention across organisations.
-- The delivery log is kept 7 days; dead letters not replayed, 30.
DELETE FROM webhook_deliveries
WHERE (created_at < @log_before AND NOT (dead_lettered AND replayed_at IS NULL)) OR created_at < @dead_before;

-- name: HourlyWebhookDeliveries :many
-- tenant: system - usage recording across organisations.
SELECT p.org_id, p.id AS project_id, o.plan_id, date_trunc('hour', d.created_at)::timestamptz AS period_start,
       count(*)::bigint AS attempts, count(*) FILTER (WHERE d.succeeded)::bigint AS successes
FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id JOIN projects p ON p.id = w.project_id
JOIN organizations o ON o.id = p.org_id
WHERE d.created_at >= @from_ts AND d.created_at < @to_ts
GROUP BY 1, 2, 3, 4;

-- ---- Scheduled jobs --------------------------------------------------------

-- name: InsertJob :one
-- tenant: system - a project the request already authorized.
INSERT INTO scheduled_jobs (id, project_id, name, cron, timezone, kind, spec_enc, timeout_s, overlap, enabled, next_run_at, created_by)
VALUES (@id, @project_id, @name, @cron, @timezone, @kind, @spec_enc, @timeout_s, @overlap, @enabled, sqlc.narg(next_run_at), sqlc.narg(created_by))
RETURNING *;

-- name: GetJob :one
-- tenant: system - a project the request already authorized.
SELECT * FROM scheduled_jobs WHERE id = @id AND project_id = @project_id;

-- name: GetJobByID :one
-- tenant: system - the scheduler.
SELECT * FROM scheduled_jobs WHERE id = @id;

-- name: ListJobs :many
-- tenant: system - a project the request already authorized.
SELECT * FROM scheduled_jobs WHERE project_id = @project_id ORDER BY name;

-- name: UpdateJob :one
-- tenant: system - a project the request already authorized.
UPDATE scheduled_jobs SET name = @name, cron = @cron, timezone = @timezone, kind = @kind, spec_enc = @spec_enc,
  timeout_s = @timeout_s, overlap = @overlap, enabled = @enabled, next_run_at = sqlc.narg(next_run_at), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: DeleteJob :exec
-- tenant: system - a project the request already authorized.
DELETE FROM scheduled_jobs WHERE id = @id;

-- name: CountOrgJobs :one
SELECT count(*)::int FROM scheduled_jobs j JOIN projects p ON p.id = j.project_id
WHERE p.org_id = @org_id AND p.deleted_at IS NULL;

-- name: DueJobs :many
-- tenant: system - the scheduler across organisations.
SELECT j.* FROM scheduled_jobs j JOIN projects p ON p.id = j.project_id
WHERE j.enabled AND j.next_run_at <= @now::timestamptz AND p.deleted_at IS NULL
ORDER BY j.next_run_at LIMIT 500;

-- name: SetJobNextRun :exec
-- tenant: system - the scheduler.
UPDATE scheduled_jobs SET next_run_at = sqlc.narg(next_run_at) WHERE id = @id;

-- name: SetJobFailures :exec
-- tenant: system - the scheduler.
UPDATE scheduled_jobs SET consecutive_failures = @consecutive_failures WHERE id = @id;

-- name: LockJob :one
-- tenant: system - the scheduler, or a job the request already authorized.
-- Serialises the overlap check and the run insert of one job across servers.
SELECT id FROM scheduled_jobs WHERE id = @id FOR UPDATE;

-- name: InsertJobRun :one
-- tenant: system - the scheduler.
INSERT INTO job_runs (job_id, scheduled_for, started_at, finished_at, status, error, trigger)
VALUES (@job_id, @scheduled_for, sqlc.narg(started_at), sqlc.narg(finished_at), @status, sqlc.narg(error), @trigger)
RETURNING *;

-- name: StartJobRun :exec
-- tenant: system - the scheduler.
UPDATE job_runs SET status = 'running', started_at = @started_at WHERE id = @id;

-- name: FinishJobRun :exec
-- tenant: system - the scheduler.
UPDATE job_runs SET status = @status, finished_at = @finished_at, rows_affected = sqlc.narg(rows_affected),
  status_code = sqlc.narg(status_code), error = sqlc.narg(error)
WHERE id = @id;

-- name: ActiveJobRuns :many
-- tenant: system - the scheduler.
SELECT * FROM job_runs WHERE job_id = @job_id AND status IN ('queued', 'running') ORDER BY id;

-- name: FailInterruptedRuns :execrows
-- tenant: system - the scheduler, at start: runs a stopped server left.
UPDATE job_runs SET status = 'failed', finished_at = now(), error = 'interrupted: the server stopped during the run'
WHERE status IN ('queued', 'running') AND scheduled_for < @before;

-- name: ListJobRuns :many
-- tenant: system - a job the request already authorized.
SELECT * FROM job_runs WHERE job_id = @job_id ORDER BY scheduled_for DESC, id DESC LIMIT @max_rows;

-- name: LastJobRun :one
-- tenant: system - a job the request already authorized.
SELECT * FROM job_runs WHERE job_id = @job_id AND status <> 'skipped' ORDER BY id DESC LIMIT 1;

-- name: SweepJobRuns :execrows
-- tenant: system - retention across organisations.
-- History is kept 30 days, at most 1,000 runs per job.
DELETE FROM job_runs r WHERE r.finished_at IS NOT NULL AND (r.scheduled_for < @before OR r.id < (
  SELECT k.id FROM job_runs k WHERE k.job_id = r.job_id ORDER BY k.id DESC OFFSET 999 LIMIT 1));

-- name: HourlyJobRuns :many
-- tenant: system - usage recording across organisations.
SELECT p.org_id, p.id AS project_id, o.plan_id, date_trunc('hour', r.scheduled_for)::timestamptz AS period_start, j.kind,
       count(*)::bigint AS runs
FROM job_runs r JOIN scheduled_jobs j ON j.id = r.job_id JOIN projects p ON p.id = j.project_id
JOIN organizations o ON o.id = p.org_id
WHERE r.scheduled_for >= @from_ts AND r.scheduled_for < @to_ts AND r.status IN ('succeeded', 'failed', 'timed_out')
GROUP BY 1, 2, 3, 4, 5;
