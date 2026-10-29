-- name: ProjectsByDBName :many
-- tenant: system - the orphan sweep compares shared clusters' databases and roles with every project, live or deleted.
-- Every project, live or deleted, whose database is one of names, and
-- whether an operation on it is still queued or running.
SELECT p.id, p.db_name, p.instance_id, p.tier, p.deleted_at,
  EXISTS (SELECT 1 FROM operations o WHERE o.project_id = p.id AND o.status IN ('queued', 'running')) AS busy
FROM projects p
WHERE p.db_name = ANY(@names::text[]);
