-- +goose Up
-- Role management on dedicated projects: the owner role gets CREATEROLE
-- (Postgres 16+ limits it to roles it creates), and the logins it creates
-- are synced here so the poolers accept them. role_name is unique across
-- the platform because each region's pooler auth file is one namespace.
ALTER TABLE projects ADD COLUMN manage_roles boolean NOT NULL DEFAULT false;

CREATE TABLE project_owned_logins (
  project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  role_name      text NOT NULL UNIQUE,
  scram_verifier text NOT NULL CHECK (scram_verifier LIKE 'SCRAM-SHA-256$%'),
  synced_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, role_name)
);

-- +goose Down
DROP TABLE project_owned_logins;
ALTER TABLE projects DROP COLUMN manage_roles;
