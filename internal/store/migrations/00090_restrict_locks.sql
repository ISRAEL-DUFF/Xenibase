-- +goose Up
-- pg_locks (over pg_lock_status()) and pg_prepared_xacts were readable by
-- PUBLIC in shared-tier databases, and list every backend's locks and
-- every prepared transaction with its database, so a tenant could see
-- which other projects were active and what they touched; pg_blocking_pids
-- and pg_safe_snapshot_blocking_pids named other databases' backends. New
-- databases get the restriction when they are created; apply_settings
-- (RestrictActivity) applies it to the existing ones, so queue one for
-- each active shared project, as 00066 did. Archived projects have no
-- database until they are restored, which creates it. A project with an
-- exclusive operation already pending is skipped (00063 allows one at a
-- time): a pending apply_settings applies the restriction itself, and for
-- any other kind the next apply_settings does; isocheck reports a database
-- still open until then.
INSERT INTO operations (kind, project_id)
SELECT 'apply_settings', p.id FROM projects p
WHERE p.tier = 'shared' AND p.status = 'active' AND p.deleted_at IS NULL AND p.lifecycle <> 'archived'
  AND NOT EXISTS (SELECT 1 FROM operations o
                  WHERE o.project_id = p.id AND o.exclusive AND o.status IN ('queued', 'running'));

-- +goose Down
SELECT 1;
