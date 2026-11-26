-- Storage metering and the nightly reconciler (V4 §5.6, §10).

-- name: StorageToMeter :many
-- tenant: system - the storage sweep: every live project's measured files and renders, metered each hour whatever its services' state.
SELECT ps.project_id, p.org_id, o.plan_id, ps.bytes, ps.transform_bytes
FROM project_storage ps JOIN projects p ON p.id = ps.project_id JOIN organizations o ON o.id = p.org_id
WHERE p.deleted_at IS NULL AND (ps.bytes > 0 OR ps.transform_bytes > 0)
ORDER BY ps.project_id;

-- name: ClaimStorageReconcile :execrows
-- tenant: system - the storage reconciler's turn: taken when its last run is older than @every_s seconds, by one server.
INSERT INTO storage_reconciler (id, last_run_at) VALUES (true, now())
ON CONFLICT (id) DO UPDATE SET last_run_at = now()
WHERE storage_reconciler.last_run_at < now() - make_interval(secs => @every_s::float8);
