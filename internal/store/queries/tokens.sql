-- name: InsertAPIToken :one
-- parent_id is the token that created this one: revoking it revokes this
-- one too (a trigger, migration 00088).
INSERT INTO api_tokens (user_id, org_id, name, token_hash, prefix, scopes, project_ids, expires_at, created_via, oauth_client_id, parent_id)
VALUES (@user_id, @org_id, @name, @token_hash, @prefix, @scopes::text[], sqlc.narg(project_ids)::uuid[], @expires_at, @created_via, sqlc.narg(oauth_client_id), sqlc.narg(parent_id))
RETURNING *;

-- name: GetTokenForAuth :one
-- tenant: system - resolving a bearer token is how a request learns its org.
SELECT t.id, t.user_id, t.org_id, t.name, t.scopes, t.project_ids, t.expires_at, t.revoked_at, t.last_used_at,
       u.email, u.name AS user_name, u.platform_role, u.disabled_at, u.approved_at, u.email_verified_at,
       o.status AS org_status
FROM api_tokens t
JOIN users u ON u.id = t.user_id
JOIN organizations o ON o.id = t.org_id
WHERE t.token_hash = @token_hash;

-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = @now::timestamptz, last_used_ip = sqlc.narg(ip)
WHERE id = @id AND org_id = @org_id AND (last_used_at IS NULL OR last_used_at < @stale_before::timestamptz);

-- name: ListUserTokens :many
-- tenant: system - a user's own tokens, across their organisations.
SELECT t.*, o.name AS org_name, c.name AS oauth_client_name
FROM api_tokens t JOIN organizations o ON o.id = t.org_id
LEFT JOIN oauth_clients c ON c.id = t.oauth_client_id
WHERE t.user_id = @user_id
ORDER BY t.revoked_at IS NOT NULL, t.created_at DESC;

-- name: ListOrgTokens :many
SELECT t.*, u.email AS user_email, u.name AS user_name, c.name AS oauth_client_name
FROM api_tokens t JOIN users u ON u.id = t.user_id
LEFT JOIN oauth_clients c ON c.id = t.oauth_client_id
WHERE t.org_id = @org_id
ORDER BY t.revoked_at IS NOT NULL, t.created_at DESC;

-- name: GetUserToken :one
-- tenant: system - the caller's own token, by id.
SELECT * FROM api_tokens WHERE id = @id AND user_id = @user_id;

-- name: GetOrgToken :one
SELECT * FROM api_tokens WHERE id = @id AND org_id = @org_id;

-- name: RevokeToken :execrows
UPDATE api_tokens SET revoked_at = now(), revoked_by = sqlc.narg(revoked_by)
WHERE id = @id AND org_id = @org_id AND revoked_at IS NULL;

-- name: RevokeUserOrgTokens :execrows
-- When someone leaves or is removed from an organisation (V2 s3.4).
UPDATE api_tokens SET revoked_at = now()
WHERE org_id = @org_id AND user_id = @user_id AND revoked_at IS NULL;

-- name: RevokeUserTokens :execrows
-- tenant: system - a disabled account loses every token it has.
UPDATE api_tokens SET revoked_at = now() WHERE user_id = @user_id AND revoked_at IS NULL;

-- name: DropProjectFromTokens :exec
-- A project moved to another organisation leaves its old org's tokens.
UPDATE api_tokens SET project_ids = array_remove(project_ids, @project_id::uuid)
WHERE org_id = @org_id AND @project_id::uuid = ANY(project_ids);

-- name: RevokeEmptiedTokens :execrows
UPDATE api_tokens SET revoked_at = now()
WHERE org_id = @org_id AND project_ids IS NOT NULL AND cardinality(project_ids) = 0 AND revoked_at IS NULL;

-- name: TokensExpiringSoon :many
-- tenant: system - the expiry reminder sweep (V2 s7.2).
SELECT t.id, t.org_id, t.name, t.prefix, t.expires_at, u.email, o.name AS org_name
FROM api_tokens t JOIN users u ON u.id = t.user_id JOIN organizations o ON o.id = t.org_id
WHERE t.revoked_at IS NULL AND t.expiry_notified_at IS NULL AND t.expires_at > @now AND t.expires_at <= @before;

-- name: MarkTokenExpiryNotified :exec
UPDATE api_tokens SET expiry_notified_at = now() WHERE id = @id AND org_id = @org_id;

-- name: InsertDeviceRequest :exec
-- tenant: system - a device login has no organisation until it is approved.
INSERT INTO device_auth_requests (device_code_hash, user_code, client_name, requested_scopes, expires_at)
VALUES (@device_code_hash, @user_code, @client_name, @requested_scopes::text[], @expires_at);

-- name: GetDeviceRequestByCode :one
-- tenant: system - a device login has no organisation until it is approved.
SELECT * FROM device_auth_requests WHERE user_code = @user_code;

-- name: GetDeviceRequest :one
-- tenant: system - a device login has no organisation until it is approved.
SELECT * FROM device_auth_requests WHERE device_code_hash = @device_code_hash;

-- name: ApproveDeviceRequest :execrows
-- tenant: system - a device login has no organisation until it is approved.
UPDATE device_auth_requests SET approved_by = @approved_by, org_id = @org_id, token_id = @token_id, sealed_token = @sealed_token
WHERE device_code_hash = @device_code_hash AND approved_by IS NULL AND denied_at IS NULL AND expires_at > now();

-- name: DenyDeviceRequest :execrows
-- tenant: system - a device login has no organisation until it is approved.
UPDATE device_auth_requests SET denied_at = now()
WHERE device_code_hash = @device_code_hash AND approved_by IS NULL AND denied_at IS NULL;

-- name: PollDeviceRequest :exec
-- tenant: system - a device login has no organisation until it is approved.
UPDATE device_auth_requests SET last_polled_at = @now::timestamptz WHERE device_code_hash = @device_code_hash;

-- name: DeleteDeviceRequest :exec
-- tenant: system - a device login has no organisation until it is approved.
DELETE FROM device_auth_requests WHERE device_code_hash = @device_code_hash;

-- name: DeleteExpiredDeviceRequests :execrows
-- tenant: system - a device login has no organisation until it is approved.
DELETE FROM device_auth_requests WHERE expires_at < now() - '1 hour'::interval;
