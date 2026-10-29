-- name: InsertEdgeCredential :one
-- tenant: system - a platform admin issues an edge's credential.
INSERT INTO edge_credentials (id, name, region, secret_enc, created_by)
VALUES (@id, @name, @region, @secret_enc, @created_by)
RETURNING *;

-- name: GetLiveEdgeCredential :one
-- tenant: system - xenibase-edge names its credential on every request.
SELECT * FROM edge_credentials WHERE id = @id AND revoked_at IS NULL;

-- name: GetLiveEdgeCredentialByName :one
-- tenant: system - issuing again for the same edge returns its credential.
SELECT * FROM edge_credentials WHERE name = @name AND revoked_at IS NULL;

-- name: ListEdgeCredentials :many
-- tenant: system - the platform admin's list.
SELECT id, name, region, created_at, last_used_at, revoked_at FROM edge_credentials ORDER BY created_at;

-- name: RevokeEdgeCredential :execrows
-- tenant: system - a platform admin revokes an edge's credential.
UPDATE edge_credentials SET revoked_at = now() WHERE name = @name AND revoked_at IS NULL;

-- name: TouchEdgeCredential :exec
-- tenant: system - when an edge's credential was last used (at most once a minute).
UPDATE edge_credentials SET last_used_at = now()
WHERE id = @id AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: ProjectRegions :many
-- tenant: system - which of the projects an edge reports on are in its credential's region.
SELECT id, region FROM projects WHERE id = ANY(@ids::uuid[]);

-- name: APIKeyProjectRegions :many
-- tenant: system - which of the keys an edge reports used are in its credential's region.
SELECT k.id, p.region FROM project_api_keys k JOIN projects p ON p.id = k.project_id WHERE k.id = ANY(@ids::uuid[]);
