-- Child tables that carry their project's org_id, for scoping without a
-- join, must agree with the project: a composite foreign key to
-- projects (id, org_id) with ON UPDATE CASCADE, so moving a project to
-- another organisation moves them with it and a copy that disagrees can't
-- be written.
--
-- Rows that disagree today are put right first (before 4.2.5 a transfer
-- left reaped_sessions, editor_preferences and dedicated_requests under the
-- old organisation). The keys are added NOT VALID, which takes a short
-- lock and doesn't scan, and 00069 validates them in a transaction of its
-- own, which doesn't block writes. audit_log and usage_records keep the
-- organisation of the time and have no key.
-- +goose Up
UPDATE project_members c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;
UPDATE project_db_users c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;
UPDATE saved_queries c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;
UPDATE reaped_sessions c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;
UPDATE editor_preferences c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;
UPDATE dedicated_requests c SET org_id = p.org_id FROM projects p WHERE p.id = c.project_id AND c.org_id <> p.org_id;

ALTER TABLE projects ADD CONSTRAINT projects_id_org_id_key UNIQUE (id, org_id);

ALTER TABLE project_members ADD CONSTRAINT project_members_project_org_fkey FOREIGN KEY (project_id, org_id)
  REFERENCES projects (id, org_id) ON UPDATE CASCADE ON DELETE CASCADE NOT VALID;
ALTER TABLE project_db_users ADD CONSTRAINT project_db_users_project_org_fkey FOREIGN KEY (project_id, org_id)
  REFERENCES projects (id, org_id) ON UPDATE CASCADE ON DELETE CASCADE NOT VALID;
ALTER TABLE saved_queries ADD CONSTRAINT saved_queries_project_org_fkey FOREIGN KEY (project_id, org_id)
  REFERENCES projects (id, org_id) ON UPDATE CASCADE ON DELETE CASCADE NOT VALID;
ALTER TABLE reaped_sessions ADD CONSTRAINT reaped_sessions_project_org_fkey FOREIGN KEY (project_id, org_id)
  REFERENCES projects (id, org_id) ON UPDATE CASCADE ON DELETE CASCADE NOT VALID;
ALTER TABLE editor_preferences ADD CONSTRAINT editor_preferences_project_org_fkey FOREIGN KEY (project_id, org_id)
  REFERENCES projects (id, org_id) ON UPDATE CASCADE ON DELETE CASCADE NOT VALID;
ALTER TABLE dedicated_requests ADD CONSTRAINT dedicated_requests_project_org_fkey FOREIGN KEY (project_id, org_id)
  REFERENCES projects (id, org_id) ON UPDATE CASCADE ON DELETE CASCADE NOT VALID;

-- +goose Down
ALTER TABLE dedicated_requests DROP CONSTRAINT dedicated_requests_project_org_fkey;
ALTER TABLE editor_preferences DROP CONSTRAINT editor_preferences_project_org_fkey;
ALTER TABLE reaped_sessions DROP CONSTRAINT reaped_sessions_project_org_fkey;
ALTER TABLE saved_queries DROP CONSTRAINT saved_queries_project_org_fkey;
ALTER TABLE project_db_users DROP CONSTRAINT project_db_users_project_org_fkey;
ALTER TABLE project_members DROP CONSTRAINT project_members_project_org_fkey;
ALTER TABLE projects DROP CONSTRAINT projects_id_org_id_key;
