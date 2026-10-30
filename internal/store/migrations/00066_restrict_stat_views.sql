-- +goose Up
-- Shared-tier databases hid pg_stat_activity from tenants but not
-- pg_stat_database(_conflicts) or the pg_stat_get_backend_* and
-- pg_stat_get_db_* functions, which show every database's and backend's
-- activity. New databases get the full restriction when they are created;
-- apply_settings applies it to the existing ones, so queue one for each
-- active shared project (as 00018 did for temp_file_limit). Archived
-- projects have no database until they are restored, which creates it.
-- A project with an exclusive operation already pending is skipped (00063
-- allows one at a time): a pending apply_settings applies the restriction
-- itself, and for any other kind the next apply_settings does; isocheck
-- reports a database still open until then.
INSERT INTO operations (kind, project_id)
SELECT 'apply_settings', p.id FROM projects p
WHERE p.tier = 'shared' AND p.status = 'active' AND p.deleted_at IS NULL AND p.lifecycle <> 'archived'
  AND NOT EXISTS (SELECT 1 FROM operations o
                  WHERE o.project_id = p.id AND o.exclusive AND o.status IN ('queued', 'running'));

-- +goose Down
SELECT 1;
