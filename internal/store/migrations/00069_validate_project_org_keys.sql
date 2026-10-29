-- Validates 00068's keys. Rows written between 00068's fix-ups and its
-- keys (by a server still running the old release) are put right first;
-- since 00068 committed, no row that disagrees can be written.
-- +goose Up
UPDATE project_members c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;
UPDATE project_db_users c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;
UPDATE saved_queries c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;
UPDATE reaped_sessions c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;
UPDATE editor_preferences c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;
UPDATE dedicated_requests c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;

ALTER TABLE project_members VALIDATE CONSTRAINT project_members_project_org_fkey;
ALTER TABLE project_db_users VALIDATE CONSTRAINT project_db_users_project_org_fkey;
ALTER TABLE saved_queries VALIDATE CONSTRAINT saved_queries_project_org_fkey;
ALTER TABLE reaped_sessions VALIDATE CONSTRAINT reaped_sessions_project_org_fkey;
ALTER TABLE editor_preferences VALIDATE CONSTRAINT editor_preferences_project_org_fkey;
ALTER TABLE dedicated_requests VALIDATE CONSTRAINT dedicated_requests_project_org_fkey;

-- +goose Down
-- A validated key can't be made NOT VALID again; 00068's Down drops it.
SELECT 1;
