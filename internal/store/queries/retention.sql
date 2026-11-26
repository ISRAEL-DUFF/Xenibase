-- How long the metadata database keeps tenants' data
-- (docs/operations.md, "What is kept").

-- name: PruneReapedSessions :execrows
-- tenant: system - retention across organisations: 30 days, and nothing of a deleted project.
-- The reaper keeps the statement's text, which is the tenant's own data.
DELETE FROM reaped_sessions r
WHERE r.created_at < @before OR r.project_id IN (SELECT p.id FROM projects p WHERE p.deleted_at IS NOT NULL);

-- name: PruneFinishedOperations :execrows
-- tenant: system - retention across organisations.
-- A finished operation older than @before loses its parameters and step
-- log, at most @max_rows at a time; the row stays as the summary (kind,
-- project, status, error, who and when). The keys the alerts and the
-- isolation checks group by (instance_id, region) are kept.
UPDATE operations o
SET params = jsonb_strip_nulls(jsonb_build_object('instance_id', o.params->'instance_id', 'region', o.params->'region'))
             || '{"pruned": true}'::jsonb,
    log = jsonb_build_array(jsonb_build_object('ts', now(), 'step', 'retention', 'level', 'info',
      'msg', 'parameters and step log removed after the retention period'))
WHERE o.id IN (
  SELECT x.id FROM operations x
  WHERE x.status IN ('succeeded', 'failed') AND NOT (x.params @> '{"pruned": true}') AND x.finished_at < @before::timestamptz
  ORDER BY x.finished_at LIMIT @max_rows::int
);
