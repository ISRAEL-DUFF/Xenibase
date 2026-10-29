-- Branches (V2 s8): shared-tier projects with a parent.

-- name: SetProjectBranch :exec
-- tenant: system - set in the transaction that creates the branch project.
UPDATE projects SET parent_project_id = @parent_project_id, branch_source = @branch_source,
  branch_schema_only = @branch_schema_only, expires_at = sqlc.narg(expires_at), sensitive_data = @sensitive_data
WHERE id = @id;

-- name: SetProjectSensitive :exec
-- tenant: system - set in the transaction that creates the project.
UPDATE projects SET sensitive_data = @sensitive_data WHERE id = @id;

-- name: ListBranches :many
SELECT * FROM projects
WHERE parent_project_id = @parent_project_id AND org_id = @org_id AND deleted_at IS NULL
ORDER BY created_at DESC;

-- name: CountLiveBranches :one
SELECT count(*)::int FROM projects WHERE parent_project_id = @parent_project_id AND org_id = @org_id AND deleted_at IS NULL;

-- name: CountOrgBranches :one
-- Branches that count towards the branches quota.
SELECT count(*)::int FROM projects WHERE org_id = @org_id AND deleted_at IS NULL AND parent_project_id IS NOT NULL;

-- name: UpdateBranch :one
-- tenant: system - a project the request already authorized.
-- Changes a branch's expiry (re-arming the 24-hour warning), and whether
-- it takes nightly backups.
UPDATE projects SET
  expires_at = CASE WHEN @set_expiry::bool THEN sqlc.narg(expires_at)::timestamptz ELSE expires_at END,
  expiry_notified_at = CASE WHEN @set_expiry::bool THEN NULL ELSE expiry_notified_at END,
  branch_backups = COALESCE(sqlc.narg(branch_backups)::bool, branch_backups)
WHERE id = @id AND parent_project_id IS NOT NULL
RETURNING *;

-- name: SetProjectSensitiveData :one
-- tenant: system - a project the request already authorized.
UPDATE projects SET sensitive_data = @sensitive_data WHERE id = @id RETURNING *;

-- name: DetachBranch :one
-- tenant: system - a project the request already authorized.
-- The branch becomes an ordinary standalone project.
UPDATE projects SET parent_project_id = NULL, expires_at = NULL, expiry_notified_at = NULL, branch_backups = false
WHERE id = @id AND parent_project_id IS NOT NULL
RETURNING *;

-- name: ExpiredBranches :many
-- tenant: system - the hourly branch expiry across organisations.
-- Live branches past their expiry with nothing in flight, in an active
-- organisation (a suspended one's expiry pauses, V2 s10.8).
SELECT p.* FROM projects p
WHERE p.parent_project_id IS NOT NULL AND p.deleted_at IS NULL AND p.expires_at IS NOT NULL AND p.expires_at <= @now::timestamptz
  AND p.status IN ('active', 'error')
  AND EXISTS (SELECT 1 FROM organizations o WHERE o.id = p.org_id AND o.status = 'active')
  AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.project_id = p.id AND o.status IN ('queued', 'running'))
ORDER BY p.expires_at
LIMIT 100;

-- name: BranchesExpiringSoon :many
-- tenant: system - the 24-hour expiry warning across organisations; rows carry org_id.
SELECT p.id, p.org_id, p.name, p.expires_at, u.email, u.name AS user_name, par.name AS parent_name
FROM projects p
JOIN users u ON u.id = p.created_by
JOIN projects par ON par.id = p.parent_project_id
WHERE p.parent_project_id IS NOT NULL AND p.deleted_at IS NULL AND p.expiry_notified_at IS NULL
  AND p.expires_at > @now::timestamptz AND p.expires_at <= @before::timestamptz
  AND u.disabled_at IS NULL
LIMIT 100;

-- name: MarkBranchExpiryNotified :exec
-- tenant: system - the 24-hour expiry warning.
UPDATE projects SET expiry_notified_at = now() WHERE id = @id;

-- name: HourlyBranches :many
-- tenant: system - usage recording; rows carry org_id.
-- The fraction of each hour from @from_ts to @last_hour each branch existed.
SELECT p.id AS project_id, p.org_id, o.plan_id, g.h::timestamptz AS period_start,
       (extract(epoch FROM LEAST(g.h + '1 hour'::interval, COALESCE(p.deleted_at, 'infinity'::timestamptz)) - GREATEST(g.h, p.created_at)) / 3600)::float8 AS fraction
FROM projects p
JOIN organizations o ON o.id = p.org_id
CROSS JOIN generate_series(@from_ts::timestamptz, @last_hour::timestamptz, '1 hour'::interval) AS g(h)
WHERE p.parent_project_id IS NOT NULL
  AND p.created_at < g.h + '1 hour'::interval AND (p.deleted_at IS NULL OR p.deleted_at > g.h);

-- name: LatestProjectSize :one
-- tenant: system - a project the request already authorized; -1 when not measured.
SELECT COALESCE((SELECT m.value FROM metric_points m
  WHERE m.scope = 'project' AND m.scope_id = @project_id AND m.metric = 'size_bytes'
  ORDER BY m.ts DESC LIMIT 1), -1)::float8;

-- name: CopyProjectMembers :exec
-- A branch starts with its parent's project members and their roles.
INSERT INTO project_members (project_id, user_id, org_id, role, added_by)
SELECT @branch_id, m.user_id, m.org_id, m.role, m.added_by FROM project_members m
WHERE m.project_id = @parent_id AND m.org_id = @org_id
ON CONFLICT (project_id, user_id) DO NOTHING;

-- name: ProjectIsBranch :one
-- tenant: system - the authorization step, after ResolveProjectOrg.
SELECT (parent_project_id IS NOT NULL)::bool FROM projects WHERE id = @id;

-- name: ResolveProject :one
-- tenant: system - the authorization step that finds a project's
-- organisation (and parent, for branches) before anything is checked.
SELECT org_id, parent_project_id FROM projects WHERE id = @id;

-- name: DeleteBranchMemberships :execrows
-- Someone removed from a project leaves its live branches too: they hold
-- its data, and their members are a copy of its own (V2 s8.1).
DELETE FROM project_members m USING projects b
WHERE b.id = m.project_id AND b.parent_project_id = @parent_id AND b.org_id = @org_id AND b.deleted_at IS NULL
  AND m.user_id = @user_id AND m.org_id = @org_id;
