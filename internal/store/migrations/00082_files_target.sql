-- +goose Up
-- V4 §5.1: a region's file storage can have a target of its own, so the
-- edges, which hold its credentials, can't reach the region's backups.
-- NULL keeps files in the region's backup target, as before (with a
-- warning and the files_share_backups alert).
ALTER TABLE regions ADD COLUMN files_target_id uuid REFERENCES storage_targets(id);

-- +goose Down
ALTER TABLE regions DROP COLUMN files_target_id;
