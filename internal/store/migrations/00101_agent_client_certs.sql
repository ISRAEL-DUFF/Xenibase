-- +goose Up
-- Every client certificate a xenibase-server issues itself for its agents
-- (agentca.ServerClientCertFor): a day long, one key pair per server. The
-- live ones, unexpired and unrevoked, are the list each agent accepts once
-- it has been given one (nodes.Service pushes it, signed by the agent CA),
-- so a copied certificate can be withdrawn by revoking its serial.
CREATE TABLE agent_client_certs (
  serial      text PRIMARY KEY,
  server_name text NOT NULL,
  issued_at   timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  revoked_at  timestamptz,
  revoked_by  uuid REFERENCES users(id) ON DELETE SET NULL
);
CREATE INDEX agent_client_certs_live ON agent_client_certs (expires_at) WHERE revoked_at IS NULL;

-- One row: the list's generation, raised in the same transaction as every
-- change to it. The row lock orders the changes, so a higher generation is
-- always a later list, and an agent refuses one older than it has.
CREATE TABLE agent_client_cert_state (
  id         boolean PRIMARY KEY DEFAULT true CHECK (id),
  generation bigint NOT NULL
);
INSERT INTO agent_client_cert_state (id, generation) VALUES (true, 0);

-- +goose Down
DROP TABLE agent_client_cert_state;
DROP TABLE agent_client_certs;
