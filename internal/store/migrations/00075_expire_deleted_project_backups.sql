-- +goose Up
-- A deleted project's nightly and archive backups never expired: retention
-- runs only after a project's next backup, which a deleted project never
-- takes, so they stayed on platform storage, listed and restorable, though
-- only the final backup is promised to outlive a deletion, for 30 days
-- (V2 s10.10). Deletes now date them with the final backup. Projects
-- already deleted get the same date, but at least a week from now, so an
-- upgrade does not remove them within the hour. Org targets are the
-- organisation's and are left alone.
UPDATE backups b
SET expires_at = GREATEST(
  COALESCE((SELECT max(f.expires_at) FROM backups f
            WHERE f.project_id = b.project_id AND f.kind = 'final' AND f.status IN ('succeeded', 'copied')),
           p.deleted_at + interval '30 days'),
  now() + interval '7 days')
FROM projects p
WHERE p.id = b.project_id AND p.deleted_at IS NOT NULL
  AND b.kind IN ('logical', 'archive') AND b.status IN ('succeeded', 'copied') AND b.expires_at IS NULL
  AND (b.storage_target_id IS NULL OR EXISTS (SELECT 1 FROM storage_targets t WHERE t.id = b.storage_target_id AND t.org_id IS NULL));

-- +goose Down
-- The dates stay: the previous release's hourly expiry honours them too.
SELECT 1;
