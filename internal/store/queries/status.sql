-- name: StatusSubscriberRows :many
-- tenant: system - the status page's managed subscribers: paying organisations' owners and billing contacts, with what their projects use.
WITH paying AS (
  SELECT o.id FROM organizations o JOIN billing_accounts b ON b.org_id = o.id
  WHERE o.status = 'active' AND b.plan <> 'free'
), uses AS (
  SELECT p.org_id, p.region, p.tier, coalesce(i.ha_enabled, false)::bool AS ha, coalesce(ps.enabled, false)::bool AS services
  FROM projects p
  LEFT JOIN instances i ON i.id = p.instance_id
  LEFT JOIN project_services ps ON ps.project_id = p.id
  WHERE p.deleted_at IS NULL AND p.org_id IN (SELECT id FROM paying)
), emails AS (
  SELECT c.org_id, lower(c.email::text) AS email FROM billing_contacts c
  WHERE c.status_emails AND c.org_id IN (SELECT id FROM paying)
  UNION
  SELECT m.org_id, lower(u.email::text) FROM org_members m JOIN users u ON u.id = m.user_id
  WHERE m.role = 'owner' AND u.disabled_at IS NULL AND m.org_id IN (SELECT id FROM paying)
    AND NOT EXISTS (SELECT 1 FROM billing_contacts c WHERE c.org_id = m.org_id AND c.email = u.email AND NOT c.status_emails)
)
SELECT e.email::text AS email, u.region::text AS region, u.tier::text AS tier, u.ha, u.services
FROM emails e JOIN uses u ON u.org_id = e.org_id
ORDER BY e.email;

-- name: SetBillingContactStatusEmails :execrows
-- tenant: system - a contact of an org the caller resolved.
UPDATE billing_contacts SET status_emails = @status_emails WHERE org_id = @org_id AND email = @email;

-- name: TouchEdge :exec
-- tenant: system - an edge's report, for its liveness.
INSERT INTO edges (name, region, last_report_at) VALUES (@name, @region, now())
ON CONFLICT (name) DO UPDATE SET region = excluded.region, last_report_at = now();

-- name: EdgeHealth :many
-- tenant: system - per region, the edges seen in the last day and those silent since a time.
SELECT region, count(*)::int AS edges, (count(*) FILTER (WHERE last_report_at < @stale_before))::int AS silent
FROM edges WHERE last_report_at > now() - interval '1 day'
GROUP BY region ORDER BY region;

-- name: ProviderUnavailableSince :many
-- tenant: system - payment providers whose last automatic charge since a time hit an outage.
SELECT DISTINCT ON (provider) provider, (error LIKE 'provider unavailable%')::bool AS unavailable
FROM payment_intents WHERE automatic AND created_at > @since AND status IN ('succeeded', 'failed')
ORDER BY provider, created_at DESC;

-- name: UnissuedDrafts :one
-- tenant: system - drafts of a month or an earlier one that aren't held and haven't been issued.
SELECT count(*)::int FROM invoices WHERE status = 'draft' AND NOT held AND period_start <= @period_start;

-- name: OrgOpenIncidents :many
-- Open incidents that affect one of the org's projects: named in their
-- scope (the project or its nodes), or, unscoped, in the project's region
-- (or all regions) on a component the project uses.
SELECT i.id, i.title, i.components, i.region_id, i.severity, i.status, i.started_at,
  coalesce((SELECT u.body FROM incident_updates u WHERE u.incident_id = i.id ORDER BY u.posted_at DESC, u.id DESC LIMIT 1), '')::text AS latest
FROM incidents i
WHERE i.resolved_at IS NULL AND i.cancelled_at IS NULL AND i.started_at <= now()
  AND NOT (i.severity = 'maintenance' AND i.announced_at IS NULL)
  AND (i.scheduled_start IS NULL OR i.scheduled_start <= now())
  AND EXISTS (
    SELECT 1 FROM projects p LEFT JOIN project_services ps ON ps.project_id = p.id
    WHERE p.org_id = @org_id AND p.deleted_at IS NULL AND maintenance_covers(i, p)
      AND (EXISTS (SELECT 1 FROM incident_scope s WHERE s.incident_id = i.id)
        OR i.components && (
          ARRAY['dashboard', 'billing', 'backups', 'webhooks-jobs', 'edge-pooler']::text[]
          || CASE WHEN p.tier = 'dedicated' THEN ARRAY['dedicated']::text[] ELSE ARRAY['shared-tier']::text[] END
          || CASE WHEN coalesce(ps.enabled, false) THEN ARRAY['backend-services']::text[] ELSE '{}'::text[] END)))
ORDER BY i.started_at DESC
LIMIT 20;

-- name: OrgBillingStanding :one
-- An org's dunning state, failing wallet charges and this month's budget alert.
SELECT b.dunning_state, (b.card_failing_since IS NOT NULL)::bool AS charge_failing, b.budget_alerted, b.budget_month
FROM billing_accounts b WHERE b.org_id = @org_id;

-- name: InsertEdgeCPUSample :exec
-- tenant: system - an edge report's CPU, for edge capacity (V4.1 §11).
INSERT INTO edge_cpu_samples (edge, region, cpu_percent) VALUES (@edge, @region, @cpu_percent);

-- name: PruneEdgeCPUSamples :exec
-- tenant: system - edge CPU is kept two days.
DELETE FROM edge_cpu_samples WHERE at < now() - interval '2 days';

-- name: EdgeCPUByRegion :many
-- tenant: system - each region's edge CPU since a time, in 5-minute buckets
-- averaged across its edges: the lowest bucket and how many there are.
SELECT region, min(avg_cpu)::float8 AS min_cpu, max(avg_cpu)::float8 AS max_cpu, count(*)::int AS buckets
FROM (
  SELECT region, date_bin('5 minutes', at, '2000-01-01') AS bucket, avg(cpu_percent) AS avg_cpu
  FROM edge_cpu_samples WHERE at >= @since GROUP BY region, bucket
) b
GROUP BY region ORDER BY region;
