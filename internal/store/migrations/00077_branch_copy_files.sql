-- +goose Up
-- V4.1 s9.5: a branch made with copy_files copies its parent's files again
-- after every reset, so it remembers the choice. Branches made before
-- take it from their create operation, while that is kept.
ALTER TABLE projects ADD COLUMN branch_copy_files boolean NOT NULL DEFAULT false;
UPDATE projects p SET branch_copy_files = true
FROM operations o
WHERE o.project_id = p.id AND o.kind = 'branch_create' AND p.parent_project_id IS NOT NULL
  AND jsonb_typeof(o.params -> 'branch' -> 'copy_files') = 'boolean' AND (o.params -> 'branch' ->> 'copy_files')::boolean;

-- +goose Down
ALTER TABLE projects DROP COLUMN branch_copy_files;
