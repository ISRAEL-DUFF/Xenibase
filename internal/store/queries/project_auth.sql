-- Auth core (V4 §4, M31): settings, queued emails, sends, active users.

-- name: GetAuthConfig :one
-- tenant: system - a project the request already authorized.
SELECT * FROM project_auth_config WHERE project_id = @project_id;

-- name: UpsertAuthConfig :one
-- tenant: system - a project the request already authorized.
INSERT INTO project_auth_config (project_id, config, providers_enc, templates)
VALUES (@project_id, @config, @providers_enc, @templates)
ON CONFLICT (project_id) DO UPDATE SET config = EXCLUDED.config, providers_enc = EXCLUDED.providers_enc,
  templates = EXCLUDED.templates, updated_at = now()
RETURNING *;

-- name: EdgeAuthConfigs :many
-- tenant: system - xenibase-edge's configuration feed: auth settings.
SELECT project_id, config, providers_enc FROM project_auth_config WHERE project_id = ANY(@project_ids::uuid[]);

-- name: EdgeSigningKeys :many
-- tenant: system - xenibase-edge's configuration feed: the active signing keys.
SELECT project_id, id, kid, private_enc FROM project_jwt_keys
WHERE project_id = ANY(@project_ids::uuid[]) AND status = 'active';

-- name: DemoteActiveJWTKey :exec
-- tenant: system - a project the request already authorized.
UPDATE project_jwt_keys SET status = 'verifying', verify_until = @verify_until
WHERE project_id = @project_id AND status = 'active';

-- name: LockJWTKey :one
-- tenant: system - a project the request already authorized.
SELECT * FROM project_jwt_keys WHERE project_id = @project_id AND id = @id FOR UPDATE;

-- name: RetireJWTKey :exec
-- tenant: system - a project the request already authorized: a key withdrawn at once.
UPDATE project_jwt_keys SET status = 'retired', retired_at = now()
WHERE project_id = @project_id AND id = @id AND status <> 'retired';

-- name: RetireJWTKeys :execrows
-- tenant: system - the signing-key sweep across all projects.
UPDATE project_jwt_keys SET status = 'retired', retired_at = now()
WHERE status = 'verifying' AND verify_until IS NOT NULL AND verify_until < now();

-- name: InsertAuthMessage :exec
-- tenant: system - a message xenibase-edge asked for, for the project it named.
INSERT INTO auth_message_outbox (id, project_id, channel, kind, via, message_enc, recipient_hash, country)
VALUES (@id, @project_id, @channel, @kind, @via, @message_enc, @recipient_hash, @country);

-- name: CountPhoneMessages :one
-- tenant: system - a project the caller resolved: SMS and WhatsApp codes since a time.
SELECT count(*) FROM auth_message_outbox WHERE project_id = @project_id AND channel <> 'email' AND created_at > @since;

-- name: CountRecipientMessages :one
-- tenant: system - a project the caller resolved: codes to one number since a time.
SELECT count(*) FROM auth_message_outbox WHERE project_id = @project_id AND recipient_hash = @recipient_hash AND created_at > @since;

-- name: CountPlatformAuthEmails :one
-- tenant: system - a project the caller resolved.
SELECT count(*) FROM auth_message_outbox WHERE project_id = @project_id AND channel = 'email' AND via = 'platform' AND created_at > @since;

-- name: ClaimAuthEmails :many
-- tenant: system - the auth email sender across all projects: due messages,
-- leased for lease_secs so no other sender takes them while they're sent.
UPDATE auth_message_outbox SET next_attempt_at = now() + make_interval(secs => @lease_secs::int)
WHERE id IN (SELECT o.id FROM auth_message_outbox o
  WHERE o.sent_at IS NULL AND o.message_enc IS NOT NULL AND o.next_attempt_at <= now()
  ORDER BY o.next_attempt_at LIMIT @lim FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: MarkAuthEmailSent :exec
-- tenant: system - the auth email sender.
UPDATE auth_message_outbox SET sent_at = now(), message_enc = NULL, attempts = attempts + 1, last_error = NULL WHERE id = @id;

-- name: MarkAuthEmailFailed :exec
-- tenant: system - the auth email sender; give_up drops the message.
UPDATE auth_message_outbox SET attempts = attempts + 1, last_error = @last_error, next_attempt_at = @next_attempt_at,
  message_enc = CASE WHEN @give_up::boolean THEN NULL ELSE message_enc END
WHERE id = @id;

-- name: PruneAuthEmails :execrows
-- tenant: system - the auth email sender across all projects.
DELETE FROM auth_message_outbox WHERE created_at < @before;

-- name: InsertMessageSend :exec
-- tenant: system - a message sent for the project the caller resolved.
INSERT INTO message_sends (project_id, channel, provider, kind, status, country, cost_minor, currency)
VALUES (@project_id, @channel, @provider, @kind, @status, @country, @cost_minor, @currency);

-- name: ProjectMessageSpend :many
-- tenant: system - a project the request already authorized: this month's platform sends and their cost.
SELECT channel, count(*)::bigint AS n, coalesce(sum(cost_minor), 0)::bigint AS cost_minor FROM message_sends
WHERE project_id = @project_id AND created_at >= @since AND status = 'sent' AND channel <> 'email'
GROUP BY channel ORDER BY channel;

-- name: InsertAuthAlert :execrows
-- tenant: system - a project the caller resolved: an alert, once a day per kind.
INSERT INTO auth_alerts (project_id, kind, day, details) VALUES (@project_id, @kind, @day, @details) ON CONFLICT DO NOTHING;

-- name: InsertAuthHook :exec
-- tenant: system - a hook event xenibase-edge sent, for the project it named.
INSERT INTO auth_hook_outbox (id, project_id, event, payload) VALUES (@id, @project_id, @event, @payload);

-- name: ClaimAuthHooks :many
-- tenant: system - the auth hook sender across all projects: due events,
-- leased for lease_secs so no other sender takes them while they're sent.
UPDATE auth_hook_outbox SET next_attempt_at = now() + make_interval(secs => @lease_secs::int)
WHERE id IN (SELECT o.id FROM auth_hook_outbox o
  WHERE o.delivered_at IS NULL AND o.failed_at IS NULL AND o.next_attempt_at <= now()
  ORDER BY o.next_attempt_at LIMIT @lim FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: MarkAuthHook :exec
-- tenant: system - the auth hook sender.
UPDATE auth_hook_outbox SET attempts = attempts + 1, last_status = @last_status, last_error = @last_error,
  delivered_at = CASE WHEN @delivered::boolean THEN now() END,
  failed_at = CASE WHEN @failed::boolean THEN now() END,
  next_attempt_at = @next_attempt_at
WHERE id = @id;

-- name: ProjectAuthHooks :many
-- tenant: system - a project the request already authorized: recent hook deliveries.
SELECT id, event, attempts, last_status, last_error, created_at, delivered_at, failed_at FROM auth_hook_outbox
WHERE project_id = @project_id ORDER BY created_at DESC LIMIT 50;

-- name: PruneAuthHooks :execrows
-- tenant: system - the auth hook sender across all projects.
DELETE FROM auth_hook_outbox WHERE created_at < @before;

-- name: ProjectMessageSends :many
-- tenant: system - a project the request already authorized.
SELECT channel, provider, status, count(*)::bigint AS n FROM message_sends
WHERE project_id = @project_id AND created_at > @since
GROUP BY channel, provider, status ORDER BY channel, provider, status;

-- name: InsertActiveUser :execrows
-- tenant: system - xenibase-edge's report, for projects it serves.
INSERT INTO auth_mau (project_id, month, user_id) VALUES (@project_id, @month, @user_id) ON CONFLICT DO NOTHING;

-- name: ProjectMAU :one
-- tenant: system - a project the request already authorized.
SELECT count(*) FROM auth_mau WHERE project_id = @project_id AND month = @month;
