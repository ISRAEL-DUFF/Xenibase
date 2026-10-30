-- +goose Up
-- A credential per xenibase-edge, bound to one region: the server serves
-- an edge holding one only that region's projects, whatever region the
-- edge asks for. The platform-wide XENIBASE_EDGE_SECRET serves only the
-- regions without their own API domain. The edge signs its requests with
-- the credential, so the server keeps it sealed with the master key
-- (associated data "edge_credentials.secret_enc:<id>"), not hashed.
CREATE TABLE edge_credentials (
  id           uuid PRIMARY KEY,
  name         text NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$'),
  region       text NOT NULL REFERENCES regions(id),
  secret_enc   bytea NOT NULL,
  created_by   uuid REFERENCES users(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,
  revoked_at   timestamptz
);
CREATE UNIQUE INDEX edge_credentials_live_name ON edge_credentials (name) WHERE revoked_at IS NULL;

-- +goose Down
DROP TABLE edge_credentials;
