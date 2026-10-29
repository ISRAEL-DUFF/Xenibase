-- Organisations, memberships, invitations, and personal database users
-- (V2 s2-3). Every query on a tenant table names the organisation it is
-- scoped to (V2 s2.6); queries that must see across organisations say why
-- with a "tenant: system" note.

-- name: InsertOrg :one
-- tenant: system - the organisation row itself, or the caller's own personal organisation.
INSERT INTO organizations (name, slug, personal_owner_id, plan_id, settings)
VALUES (@name, @slug, sqlc.narg(personal_owner_id), @plan_id, @settings)
RETURNING *;

-- name: GetPlanByName :one
SELECT * FROM quota_plans WHERE name = @name;

-- name: GetOrg :one
SELECT * FROM organizations WHERE id = @org_id AND status <> 'deleted';

-- name: GetOrgBySlug :one
-- tenant: system - the organisation row itself, or the caller's own personal organisation.
SELECT * FROM organizations WHERE slug = @slug AND status <> 'deleted';

-- name: GetPersonalOrg :one
-- tenant: system - the organisation row itself, or the caller's own personal organisation.
SELECT * FROM organizations WHERE personal_owner_id = @user_id;

-- name: UpdateOrg :one
UPDATE organizations SET name = @name, slug = @slug, settings = @settings
WHERE id = @org_id
RETURNING *;

-- name: OrgSlugTaken :one
SELECT EXISTS (SELECT 1 FROM organizations WHERE slug = @slug AND id <> @org_id);

-- name: ListUserOrgs :many
-- tenant: system - scoped by the user's own memberships (m.user_id).
-- The organisations a user belongs to, personal first.
SELECT o.*, m.role AS member_role,
       (SELECT count(*) FROM org_members x WHERE x.org_id = o.id)::int AS member_count,
       (SELECT count(*) FROM projects p WHERE p.org_id = o.id AND p.deleted_at IS NULL)::int AS project_count
FROM organizations o JOIN org_members m ON m.org_id = o.id
WHERE m.user_id = @user_id AND o.status <> 'deleted'
ORDER BY (o.personal_owner_id = @user_id) DESC NULLS LAST, o.name, o.id;

-- name: OrphanOrgs :many
-- tenant: system - organisations with no members (projects that predate
-- every user), adopted by the first platform admin at setup.
SELECT o.id FROM organizations o
WHERE NOT EXISTS (SELECT 1 FROM org_members m WHERE m.org_id = o.id) AND o.status <> 'deleted';

-- name: GetOrgMember :one
SELECT * FROM org_members WHERE org_id = @org_id AND user_id = @user_id;

-- name: InsertOrgMember :exec
-- Adds a member; an existing member keeps the higher of the two roles.
INSERT INTO org_members (org_id, user_id, role) VALUES (@org_id, @user_id, @role)
ON CONFLICT (org_id, user_id) DO UPDATE SET role = CASE
  WHEN org_members.role = 'owner' OR EXCLUDED.role = 'owner' THEN 'owner'
  WHEN org_members.role = 'admin' OR EXCLUDED.role = 'admin' THEN 'admin'
  ELSE 'member' END;

-- name: SetOrgMemberRole :execrows
UPDATE org_members SET role = @role WHERE org_id = @org_id AND user_id = @user_id;

-- name: DeleteOrgMember :execrows
DELETE FROM org_members WHERE org_id = @org_id AND user_id = @user_id;

-- name: CountOrgOwners :one
SELECT count(*) FROM org_members WHERE org_id = @org_id AND role = 'owner';

-- name: LockOrgMembers :exec
-- Serializes membership changes in one organisation (the last-owner rule).
SELECT pg_advisory_xact_lock(hashtext('xenibase_org_members:' || @org_key::text));

-- name: ListOrgMembers :many
SELECT m.org_id, m.user_id, m.role, m.created_at, u.email, u.name, u.last_active_at, u.disabled_at,
       (u.totp_secret IS NOT NULL)::bool AS totp_enabled
FROM org_members m JOIN users u ON u.id = m.user_id
WHERE m.org_id = @org_id
ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, u.email;

-- name: ListOrgProjectMemberships :many
SELECT pm.user_id, pm.project_id, pm.role, p.name AS project_name
FROM project_members pm JOIN projects p ON p.id = pm.project_id
WHERE pm.org_id = @org_id AND p.org_id = @org_id AND p.deleted_at IS NULL
ORDER BY p.name;

-- ---- Projects in an organisation ------------------------------------------

-- name: ResolveProjectOrg :one
-- tenant: system - the authorization step that finds a project's
-- organisation before anything is checked against it.
SELECT org_id FROM projects WHERE id = @id;

-- name: GetOrgProject :one
SELECT * FROM projects WHERE id = @id AND org_id = @org_id;

-- name: ListOrgProjects :many
-- Live projects in an organisation that @user_id can see: every one for
-- org owners and admins (@see_all), otherwise the ones they are a member of.
SELECT p.* FROM projects p
WHERE p.org_id = @org_id AND p.deleted_at IS NULL
  AND (sqlc.narg(status)::text IS NULL OR p.status = sqlc.narg(status))
  AND (@see_all::bool OR EXISTS (
        SELECT 1 FROM project_members pm
        WHERE pm.project_id = p.id AND pm.org_id = @org_id AND pm.user_id = @user_id))
ORDER BY p.created_at DESC, p.id DESC
LIMIT @max_rows;

-- name: SetProjectOrg :exec
-- Moves a project to another organisation (project transfer).
UPDATE projects SET org_id = @new_org_id WHERE id = @id AND org_id = @org_id;

-- ---- Project members -------------------------------------------------------

-- name: GetProjectMember :one
SELECT * FROM project_members WHERE project_id = @project_id AND user_id = @user_id AND org_id = @org_id;

-- name: UpsertProjectMember :exec
INSERT INTO project_members (project_id, user_id, org_id, role, added_by)
VALUES (@project_id, @user_id, @org_id, @role, sqlc.narg(added_by))
ON CONFLICT (project_id, user_id) DO UPDATE SET role = EXCLUDED.role, org_id = EXCLUDED.org_id;

-- name: DeleteProjectMember :execrows
DELETE FROM project_members WHERE project_id = @project_id AND user_id = @user_id AND org_id = @org_id;

-- name: DeleteOrgProjectMemberships :many
-- Removes a user from every project of an organisation.
DELETE FROM project_members WHERE org_id = @org_id AND user_id = @user_id RETURNING project_id;

-- name: ListProjectMembers :many
SELECT pm.project_id, pm.user_id, pm.role, pm.created_at, u.email, u.name, m.role AS org_role
FROM project_members pm
JOIN users u ON u.id = pm.user_id
LEFT JOIN org_members m ON m.org_id = pm.org_id AND m.user_id = pm.user_id
WHERE pm.project_id = @project_id AND pm.org_id = @org_id
ORDER BY u.email;

-- name: MoveProjectMembers :exec
UPDATE project_members SET org_id = @new_org_id WHERE project_id = @project_id AND org_id = @org_id;

-- ---- Invitations -----------------------------------------------------------

-- name: InsertInvitation :one
INSERT INTO invitations (email, token_hash, kind, org_id, org_role, project_roles, invited_by, expires_at)
VALUES (@email, @token_hash, @kind, sqlc.narg(org_id), sqlc.narg(org_role), @project_roles, @invited_by, @expires_at)
RETURNING *;

-- name: GetInvitationByToken :one
-- tenant: system - an invitation is found by its secret before anyone signs in.
SELECT i.*, o.name AS org_name, u.email AS inviter_email, u.name AS inviter_name
FROM invitations i
LEFT JOIN organizations o ON o.id = i.org_id
JOIN users u ON u.id = i.invited_by
WHERE i.token_hash = @token_hash;

-- name: GetInvitation :one
-- tenant: system - the invitee's own invitations are looked up by address
-- (ListInvitationsForEmail), then by id.
SELECT * FROM invitations WHERE id = @id;

-- name: ListOrgInvitations :many
SELECT i.*, u.email AS inviter_email
FROM invitations i JOIN users u ON u.id = i.invited_by
WHERE i.org_id = @org_id AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()
ORDER BY i.created_at DESC;

-- name: ListPlatformInvitations :many
-- tenant: system - platform invitations belong to no organisation.
SELECT i.*, u.email AS inviter_email
FROM invitations i JOIN users u ON u.id = i.invited_by
WHERE i.kind = 'platform' AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()
ORDER BY i.created_at DESC;

-- name: ListInvitationsForEmail :many
-- tenant: system - what a signed-in user has been invited to, by their address.
SELECT i.*, o.name AS org_name, u.email AS inviter_email
FROM invitations i
LEFT JOIN organizations o ON o.id = i.org_id
JOIN users u ON u.id = i.invited_by
WHERE i.email = @email AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()
ORDER BY i.created_at DESC;

-- name: RevokeOrgInvitation :execrows
UPDATE invitations SET revoked_at = now()
WHERE id = @id AND org_id = @org_id AND accepted_at IS NULL AND revoked_at IS NULL;

-- name: RevokePlatformInvitation :execrows
-- tenant: system - platform invitations belong to no organisation.
UPDATE invitations SET revoked_at = now()
WHERE id = @id AND kind = 'platform' AND accepted_at IS NULL AND revoked_at IS NULL;

-- name: MarkInvitationAccepted :execrows
-- tenant: system - accepting is the invitee's act, checked against the token or address.
UPDATE invitations SET accepted_at = now(), accepted_by = @user_id
WHERE id = @id AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now();

-- ---- Personal database users -----------------------------------------------

-- name: GetProjectDBUser :one
SELECT * FROM project_db_users WHERE project_id = @project_id AND user_id = @user_id AND org_id = @org_id;

-- name: UpsertProjectDBUser :one
INSERT INTO project_db_users (project_id, user_id, org_id, role_name, scram_verifier, access)
VALUES (@project_id, @user_id, @org_id, @role_name, @scram_verifier, @access)
ON CONFLICT (project_id, user_id) DO UPDATE
  SET scram_verifier = EXCLUDED.scram_verifier, access = EXCLUDED.access, rotated_at = now()
RETURNING *;

-- name: SetProjectDBUserAccess :exec
UPDATE project_db_users SET access = @access WHERE project_id = @project_id AND user_id = @user_id AND org_id = @org_id;

-- name: DeleteProjectDBUser :execrows
DELETE FROM project_db_users WHERE project_id = @project_id AND user_id = @user_id AND org_id = @org_id;

-- name: ListProjectDBUsers :many
-- tenant: system - the roles a tier move or restore recreates on the
-- project's own database, whichever organisation it belongs to.
SELECT * FROM project_db_users WHERE project_id = @project_id ORDER BY role_name;

-- name: ListUserDBUsersInOrg :many
SELECT * FROM project_db_users WHERE org_id = @org_id AND user_id = @user_id;

-- name: MoveProjectDBUsers :exec
UPDATE project_db_users SET org_id = @new_org_id WHERE project_id = @project_id AND org_id = @org_id;

-- name: PoolerDBUsers :many
-- tenant: system - every personal login the poolers must accept.
SELECT d.role_name, d.scram_verifier, p.region, p.forward_region, p.forward_until
FROM project_db_users d JOIN projects p ON p.id = d.project_id
WHERE p.deleted_at IS NULL AND p.status IN ('provisioning', 'active', 'promoting', 'demoting', 'moving', 'upgrading', 'restoring')
ORDER BY d.role_name;

-- name: ListOrgBackups :many
-- Backups of an organisation's live and deleted projects; members see
-- only their projects' (@see_all false: @project_ids).
SELECT b.*, p.name AS project_name, (p.deleted_at IS NOT NULL)::bool AS project_deleted,
       t.name AS target_name, t.org_id AS target_org_id, k.fingerprint AS key_fingerprint
FROM backups b JOIN projects p ON p.id = b.project_id
LEFT JOIN storage_targets t ON t.id = b.storage_target_id
LEFT JOIN backup_keys k ON k.id = b.encryption_key_id
WHERE p.org_id = @org_id AND b.status <> 'deleted'
  AND (sqlc.narg(kind)::text IS NULL OR b.kind = sqlc.narg(kind))
  AND (sqlc.narg(project_id)::uuid IS NULL OR b.project_id = sqlc.narg(project_id))
  AND (@see_all::bool OR b.project_id = ANY(@project_ids::uuid[]))
ORDER BY b.started_at DESC
LIMIT @max_rows;

-- name: ListOrgOperations :many
SELECT o.* FROM operations o JOIN projects p ON p.id = o.project_id
WHERE p.org_id = @org_id
  AND (sqlc.narg(status)::text IS NULL OR o.status = sqlc.narg(status))
  AND (sqlc.narg(kind)::text IS NULL OR o.kind = sqlc.narg(kind))
  AND (sqlc.narg(project_id)::uuid IS NULL OR o.project_id = sqlc.narg(project_id))
  AND (@see_all::bool OR o.project_id = ANY(@project_ids::uuid[]))
ORDER BY o.created_at DESC, o.id DESC
LIMIT @max_rows;

-- name: ListPlatformOperations :many
-- tenant: system - platform work (nodes, restore tests, metadata backups) has no project.
SELECT * FROM operations
WHERE project_id IS NULL
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind))
ORDER BY created_at DESC, id DESC
LIMIT @max_rows;

-- name: RenameProjectDBUser :exec
-- tenant: system - the rename_opaque operation renames a project's own logins.
UPDATE project_db_users SET role_name = @new_name WHERE project_id = @project_id AND role_name = @old_name;
