-- +goose Up
-- One exclusive operation per project at a time, in the schema (it was
-- only checked under the project's row lock, and some paths skipped the
-- check). Background operations (retrying a login drop, copying a new
-- branch's files, a restore test, dropping a failed create's leftovers)
-- may be queued beside one, some by that operation itself; they have
-- exclusive = false. The row-lock check still counts them as busy.
ALTER TABLE operations ADD COLUMN exclusive boolean NOT NULL DEFAULT true;

UPDATE operations SET exclusive = false
WHERE kind IN ('drop_db_user', 'copy_branch_files', 'restore_test', 'drop_project_objects');

-- Pending operations that already overlap (the races this closes) keep
-- running; all but the oldest on each project are no longer exclusive.
UPDATE operations o SET exclusive = false
FROM (
  SELECT id, row_number() OVER (PARTITION BY project_id ORDER BY created_at, id) AS n
  FROM operations
  WHERE exclusive AND project_id IS NOT NULL AND status IN ('queued', 'running')
) d
WHERE o.id = d.id AND d.n > 1;

CREATE UNIQUE INDEX operations_one_exclusive_per_project ON operations (project_id)
  WHERE exclusive AND project_id IS NOT NULL AND status IN ('queued', 'running');

-- +goose Down
DROP INDEX operations_one_exclusive_per_project;
ALTER TABLE operations DROP COLUMN exclusive;
