-- +goose Up
-- Shared-tier databases hid pg_stat_activity from tenants but not
-- pg_stat_database(_conflicts) or the pg_stat_get_backend_* and
-- pg_stat_get_db_* functions, which show every database's and backend's
-- activity. New databases get the full restriction when they are created;
-- apply_settings applies it to the existing ones, so queue one for each
-- active shared project (as 00018 did for temp_file_limit). Archived
-- projects have no database until they are restored, which creates it.
INSERT INTO operations (kind, project_id)
SELECT 'apply_settings', id FROM projects
WHERE tier = 'shared' AND status = 'active' AND deleted_at IS NULL AND lifecycle <> 'archived';

-- +goose Down
SELECT 1;
