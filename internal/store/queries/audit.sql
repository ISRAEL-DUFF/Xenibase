-- name: InsertAudit :exec
-- created_at is set when a row is retried after a failed write, so it
-- keeps the time of the request; NULL means now.
INSERT INTO audit_log (user_id, action, target_type, target_id, detail, ip, user_agent, outcome,
                       actor_kind, token_id, org_id, project_id, break_glass, created_at)
VALUES (sqlc.narg(user_id), @action, sqlc.narg(target_type), sqlc.narg(target_id), @detail, sqlc.narg(ip),
        sqlc.narg(user_agent), @outcome, @actor_kind, sqlc.narg(token_id), sqlc.narg(org_id), sqlc.narg(project_id), @break_glass,
        COALESCE(sqlc.narg(created_at)::timestamptz, now()));

-- name: ListAudit :many
-- One scope at a time: an org's log (@org_id), a project's slice of it
-- (@org_id and @project_id), or the platform log (@platform: org-less
-- events, plus break-glass actions wherever they happened).
SELECT a.*, u.email AS user_email
FROM audit_log a LEFT JOIN users u ON u.id = a.user_id
WHERE CASE WHEN @platform::bool THEN (a.org_id IS NULL OR a.break_glass)
           ELSE a.org_id = sqlc.narg(org_id) END
  AND (sqlc.narg(project_id)::uuid IS NULL OR a.project_id = sqlc.narg(project_id))
  AND (sqlc.narg(action)::text IS NULL OR a.action LIKE sqlc.narg(action) || '%')
  AND (sqlc.narg(outcome)::text IS NULL OR a.outcome = sqlc.narg(outcome))
  AND (sqlc.narg(target_id)::text IS NULL OR a.target_id = sqlc.narg(target_id))
  AND (sqlc.narg(before_id)::bigint IS NULL OR a.id < sqlc.narg(before_id))
ORDER BY a.id DESC
LIMIT @max_rows;
