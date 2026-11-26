-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: InsertUser :one
INSERT INTO users (email, password_hash, name, platform_role, email_verified_at, approved_at, signup_ip)
VALUES (@email, @password_hash, sqlc.narg(name), @platform_role, sqlc.narg(email_verified_at), sqlc.narg(approved_at), sqlc.narg(signup_ip))
RETURNING *;

-- name: CountSignupsFrom :one
-- Accounts created from an IP address since a time (V3 §7.4).
SELECT count(*) FROM users WHERE signup_ip = @ip AND created_at >= @since;

-- name: CountPlatformAdmins :one
SELECT count(*) FROM users WHERE platform_role = 'platform_admin' AND disabled_at IS NULL;

-- name: SetUserTOTP :exec
-- Enrols TOTP (and its recovery codes) once: an enrolled account keeps its secret.
UPDATE users SET totp_secret = @totp_secret, totp_last_step = @step, recovery_codes = @recovery_codes
WHERE id = @id;

-- name: ResetUserTOTP :exec
UPDATE users SET totp_secret = NULL, totp_last_step = 0, recovery_codes = NULL WHERE id = @id;

-- name: SetRecoveryCodes :exec
UPDATE users SET recovery_codes = @recovery_codes WHERE id = @id;

-- name: SetUserPassword :exec
UPDATE users SET password_hash = @password_hash, failed_logins = 0, locked_until = NULL WHERE id = @id;

-- name: MarkEmailVerified :exec
UPDATE users SET email_verified_at = COALESCE(email_verified_at, now()) WHERE id = @id;

-- name: UpdateUserProfile :one
UPDATE users SET name = sqlc.narg(name) WHERE id = @id RETURNING *;

-- name: ApproveUser :one
UPDATE users SET approved_at = COALESCE(approved_at, now()) WHERE id = @id RETURNING *;

-- name: SetUserDisabled :one
UPDATE users SET disabled_at = CASE WHEN @disabled::bool THEN COALESCE(disabled_at, now()) ELSE NULL END
WHERE id = @id RETURNING *;

-- name: TouchUserActivity :exec
UPDATE users SET last_active_at = now()
WHERE id = @id AND (last_active_at IS NULL OR last_active_at < now() - interval '5 minutes');

-- name: ListUsers :many
-- tenant: system - the platform admin's user list.
SELECT u.*, (SELECT count(*) FROM org_members m WHERE m.user_id = u.id)::int AS org_count
FROM users u
WHERE (sqlc.narg(query)::text IS NULL OR u.email ILIKE '%' || sqlc.narg(query) || '%' OR u.name ILIKE '%' || sqlc.narg(query) || '%')
  AND (NOT @pending_only::bool OR (u.approved_at IS NULL AND u.disabled_at IS NULL))
ORDER BY u.created_at DESC
LIMIT @max_rows;

-- name: ListUserSessions :many
SELECT id, created_at, last_seen_at, ip, user_agent FROM sessions WHERE user_id = @user_id ORDER BY last_seen_at DESC;

-- name: DeleteUserSession :execrows
DELETE FROM sessions WHERE user_id = @user_id AND id = @id;

-- name: DeleteUserSessions :exec
-- Ends every session of a user (except @keep, the caller's own, when set).
DELETE FROM sessions WHERE user_id = @user_id AND (sqlc.narg(keep)::text IS NULL OR id <> sqlc.narg(keep));

-- name: InsertEmailToken :exec
INSERT INTO email_tokens (token_hash, user_id, purpose, expires_at) VALUES (@token_hash, @user_id, @purpose, @expires_at);

-- name: InvalidateEmailTokens :exec
UPDATE email_tokens SET used_at = now() WHERE user_id = @user_id AND purpose = @purpose AND used_at IS NULL;

-- name: ConsumeEmailToken :one
UPDATE email_tokens SET used_at = now()
WHERE token_hash = @token_hash AND purpose = @purpose AND used_at IS NULL AND expires_at > now()
RETURNING user_id;

-- name: LatestTerms :one
SELECT * FROM terms_versions ORDER BY version DESC LIMIT 1;

-- name: InsertTerms :one
INSERT INTO terms_versions (version, terms_md, privacy_md, aup_md, published_by)
VALUES ((SELECT COALESCE(max(version), 0) + 1 FROM terms_versions), @terms_md, @privacy_md, @aup_md, sqlc.narg(published_by))
RETURNING *;

-- name: AcceptTerms :exec
INSERT INTO terms_acceptances (user_id, version, ip) VALUES (@user_id, @version, sqlc.narg(ip))
ON CONFLICT DO NOTHING;

-- name: AcceptedTermsVersion :one
SELECT COALESCE(max(version), 0)::int FROM terms_acceptances WHERE user_id = @user_id;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = @email;

-- name: GetUser :one
SELECT * FROM users WHERE id = @id;

-- name: RecordLoginFailure :one
-- Counts a failed password or TOTP attempt. The threshold-th failure in a
-- row locks the account for lock_seconds, and each further one for twice
-- as long as the last, up to max_lock_seconds.
UPDATE users
SET failed_logins = failed_logins + 1,
    locked_until  = CASE WHEN failed_logins + 1 >= (@threshold::int)
      THEN (@now::timestamptz) + interval '1 second' * least((@lock_seconds::float8) * power(2, least(failed_logins + 1 - (@threshold::int), 30)), (@max_lock_seconds::float8))
      ELSE locked_until END
WHERE id = @id
RETURNING failed_logins, locked_until;

-- name: ClaimLockoutNotice :execrows
-- Claims the lockout email for an account, at most once every every_seconds.
UPDATE users SET lockout_notified_at = (@now::timestamptz)
WHERE id = @id AND (lockout_notified_at IS NULL OR lockout_notified_at <= (@now::timestamptz) - interval '1 second' * (@every_seconds::float8));

-- name: GetAddressLock :one
-- When a sign-in address with no enabled account is locked until.
SELECT locked_until FROM login_address_failures WHERE email = @email;

-- name: RecordAddressFailure :one
-- Counts a failed sign-in for an address with no enabled account, and locks
-- it as RecordLoginFailure locks an account.
INSERT INTO login_address_failures AS f (email, failed_logins, locked_until, updated_at)
VALUES (@email, 1, CASE WHEN 1 >= (@threshold::int) THEN (@now::timestamptz) + interval '1 second' * least((@lock_seconds::float8), (@max_lock_seconds::float8)) END, (@now::timestamptz))
ON CONFLICT (email) DO UPDATE
SET failed_logins = f.failed_logins + 1,
    locked_until  = CASE WHEN f.failed_logins + 1 >= (@threshold::int)
      THEN (@now::timestamptz) + interval '1 second' * least((@lock_seconds::float8) * power(2, least(f.failed_logins + 1 - (@threshold::int), 30)), (@max_lock_seconds::float8))
      ELSE f.locked_until END,
    updated_at    = (@now::timestamptz)
RETURNING failed_logins, locked_until;

-- name: DeleteStaleAddressFailures :exec
DELETE FROM login_address_failures
WHERE updated_at < (@before::timestamptz) AND (locked_until IS NULL OR locked_until <= (@now::timestamptz));

-- name: ResetLoginFailures :exec
UPDATE users SET failed_logins = 0, locked_until = NULL WHERE id = @id;

-- name: AdvanceTOTPStep :execrows
-- Accepts a TOTP step only once: replayed or older codes match no row.
UPDATE users SET totp_last_step = @step WHERE id = @id AND totp_last_step < @step;

-- name: InsertChallenge :exec
INSERT INTO auth_challenges (id, kind, user_id, payload, expires_at)
VALUES (@id, @kind, sqlc.narg(user_id), @payload, @expires_at);

-- name: GetChallenge :one
SELECT * FROM auth_challenges WHERE id = @id AND kind = @kind AND expires_at > now();

-- name: BumpChallengeAttempts :one
UPDATE auth_challenges SET attempts = attempts + 1 WHERE id = @id RETURNING attempts;

-- name: DeleteChallenge :exec
DELETE FROM auth_challenges WHERE id = @id;

-- name: DeleteExpiredChallenges :exec
DELETE FROM auth_challenges WHERE expires_at <= now();

-- Session timestamps come from the auth service's clock, which is also
-- what checks them.
-- name: InsertSession :exec
INSERT INTO sessions (id, user_id, ip, user_agent, created_at, last_seen_at, reauth_at)
VALUES (@id, @user_id, sqlc.narg(ip), sqlc.narg(user_agent), @now, @now, @now);

-- name: GetSession :one
SELECT s.id, s.user_id, s.created_at, s.last_seen_at, s.reauth_at,
       o.email, o.platform_role, o.disabled_at, o.email_verified_at, o.approved_at, o.name
FROM sessions s JOIN users o ON o.id = s.user_id
WHERE s.id = @id;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = @now WHERE id = @id;

-- name: SetSessionReauth :exec
UPDATE sessions SET reauth_at = @now::timestamptz WHERE id = @id;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = @id;

-- name: DeleteIdleSessions :exec
DELETE FROM sessions WHERE last_seen_at < @idle_before OR created_at < @created_before;

-- name: LockPlatformRoles :exec
-- Serialises platform role changes, so two admins can't each demote the other.
SELECT pg_advisory_xact_lock(7003001);

-- name: SetUserPlatformRole :one
UPDATE users SET platform_role = @role WHERE id = @id RETURNING *;

-- name: ListPlatformAdmins :many
-- tenant: system - the platform's own administrators.
SELECT * FROM users WHERE platform_role = 'platform_admin' ORDER BY created_at;
