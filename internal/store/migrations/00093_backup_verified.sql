-- +goose Up
-- When a backup was last restored and read back successfully. An archive
-- drops the project's database only after its archive backup has it: a
-- retry that finds the backup without it (the server stopped during the
-- verification, or deleting the failed backup failed) verifies it again
-- first. Backups taken before this migration have none; only an archive
-- still in progress reads it, and it verifies again.
ALTER TABLE backups ADD COLUMN verified_at timestamptz;

-- +goose Down
ALTER TABLE backups DROP COLUMN verified_at;
