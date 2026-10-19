-- OAuth 2.0 for third-party apps (RFC 6749 s4.1 with PKCE, RFC 7636).

-- name: InsertOAuthClient :one
INSERT INTO oauth_clients (client_id, name, client_secret_hash, redirect_uris, scopes_allowed, created_by)
VALUES (@client_id, @name, sqlc.narg(client_secret_hash), @redirect_uris::text[], @scopes_allowed::text[], sqlc.narg(created_by))
RETURNING *;

-- name: ListOAuthClients :many
SELECT * FROM oauth_clients ORDER BY disabled_at IS NOT NULL, created_at DESC, id;

-- name: GetOAuthClient :one
SELECT * FROM oauth_clients WHERE client_id = @client_id;

-- name: DisableOAuthClient :execrows
UPDATE oauth_clients SET disabled_at = now() WHERE client_id = @client_id AND disabled_at IS NULL;

-- name: InsertOAuthCode :exec
INSERT INTO oauth_codes (code_hash, client_id, user_id, org_id, project_ids, scopes, expires_in_days, redirect_uri, code_challenge, expires_at)
VALUES (@code_hash, @client_id, @user_id, @org_id, sqlc.narg(project_ids)::uuid[], @scopes::text[], sqlc.narg(expires_in_days), @redirect_uri, @code_challenge, @expires_at);

-- name: GetOAuthCodeForUpdate :one
-- tenant: system - the exchange learns the organisation from the code itself.
SELECT * FROM oauth_codes WHERE code_hash = @code_hash FOR UPDATE;

-- name: UseOAuthCode :execrows
UPDATE oauth_codes SET used_at = now(), token_id = @token_id
WHERE code_hash = @code_hash AND org_id = @org_id AND used_at IS NULL;

-- name: DeleteExpiredOAuthCodes :execrows
-- tenant: system - the sweep clears every organisation's stale codes.
DELETE FROM oauth_codes WHERE expires_at < now() - '1 hour'::interval;

-- name: RevokeOAuthClientTokens :execrows
-- tenant: system - disabling an app revokes the tokens it holds in every organisation.
UPDATE api_tokens SET revoked_at = now(), revoked_by = sqlc.narg(revoked_by)
WHERE oauth_client_id = @oauth_client_id AND revoked_at IS NULL;

-- name: GetOAuthClientToken :one
-- tenant: system - an app revoking a token it was issued (RFC 7009) identifies it by the secret.
SELECT * FROM api_tokens WHERE token_hash = @token_hash AND oauth_client_id = @oauth_client_id;
