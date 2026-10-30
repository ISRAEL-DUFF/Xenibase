-- name: BumpAgentClientCertGeneration :one
-- tenant: system - the platform's own agent client certificates belong to no organisation.
-- Raises the allow list's generation; the row lock orders concurrent changes.
UPDATE agent_client_cert_state SET generation = generation + 1 WHERE id RETURNING generation;

-- name: AgentClientCertGeneration :one
-- tenant: system - the platform's own agent client certificates belong to no organisation.
SELECT generation FROM agent_client_cert_state WHERE id;

-- name: InsertAgentClientCert :exec
-- tenant: system - the platform's own agent client certificates belong to no organisation.
INSERT INTO agent_client_certs (serial, server_name, issued_at, expires_at)
VALUES (@serial, @server_name, @issued_at, @expires_at);

-- name: PruneAgentClientCerts :execrows
-- tenant: system - the platform's own agent client certificates belong to no organisation.
-- Forgets certificates that expired a month ago.
DELETE FROM agent_client_certs WHERE expires_at < now() - interval '30 days';

-- name: LiveAgentClientCerts :many
-- tenant: system - the platform's own agent client certificates belong to no organisation.
-- The allow list agents are given: unexpired and unrevoked.
SELECT * FROM agent_client_certs
WHERE revoked_at IS NULL AND expires_at > now()
ORDER BY serial;

-- name: ListAgentClientCerts :many
-- tenant: system - the platform admin's view of the platform's own agent client certificates.
SELECT * FROM agent_client_certs
WHERE expires_at > now() - interval '7 days'
ORDER BY issued_at DESC;

-- name: GetAgentClientCert :one
-- tenant: system - the platform's own agent client certificates belong to no organisation.
SELECT * FROM agent_client_certs WHERE serial = @serial;

-- name: RevokeAgentClientCerts :many
-- tenant: system - the platform admin revokes the platform's own agent client certificates.
-- Revokes one serial, or every live certificate of one server.
UPDATE agent_client_certs SET revoked_at = now(), revoked_by = sqlc.narg('revoked_by')
WHERE revoked_at IS NULL AND expires_at > now()
  AND (serial = sqlc.narg('serial') OR server_name = sqlc.narg('server_name'))
RETURNING *;
