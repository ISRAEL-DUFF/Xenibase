-- +goose Up
-- OAuth 2.0 for third-party apps (RFC 6749 s4.1, with PKCE, RFC 7636):
-- a platform admin registers a client; a signed-in user's consent stores a
-- short-lived authorization code; the exchange issues an ordinary API
-- token (created_via 'oauth') that remembers the app it was issued to.

CREATE TABLE oauth_clients (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  client_id          text UNIQUE NOT NULL,
  name               text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
  -- sha256 hex of the xbc_ secret, shown once; NULL for a public client.
  client_secret_hash text,
  redirect_uris      text[] NOT NULL CHECK (cardinality(redirect_uris) > 0),
  scopes_allowed     text[] NOT NULL DEFAULT ARRAY['read', 'write']
                     CHECK (cardinality(scopes_allowed) > 0 AND scopes_allowed <@ ARRAY['read', 'write', 'admin']),
  created_by         uuid REFERENCES users(id) ON DELETE SET NULL,
  created_at         timestamptz NOT NULL DEFAULT now(),
  disabled_at        timestamptz
);

-- A consent, until the app exchanges it (once, within 10 minutes).
CREATE TABLE oauth_codes (
  code_hash       text PRIMARY KEY,   -- sha256 hex
  client_id       text NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
  user_id         uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  org_id          uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  project_ids     uuid[],             -- NULL = every project the user can access in org_id
  scopes          text[] NOT NULL CHECK (cardinality(scopes) > 0 AND scopes <@ ARRAY['read', 'write', 'admin']),
  expires_in_days int,
  redirect_uri    text NOT NULL,
  code_challenge  text NOT NULL,
  -- The token the code issued; a second use revokes it (RFC 6749 s4.1.2).
  token_id        uuid REFERENCES api_tokens(id) ON DELETE SET NULL,
  expires_at      timestamptz NOT NULL,
  used_at         timestamptz,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX oauth_codes_expires ON oauth_codes (expires_at);

ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_created_via_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_created_via_check
  CHECK (created_via IN ('ui', 'device', 'api', 'oauth'));
ALTER TABLE api_tokens ADD COLUMN oauth_client_id uuid REFERENCES oauth_clients(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE api_tokens DROP COLUMN oauth_client_id;
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_created_via_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_created_via_check
  CHECK (created_via IN ('ui', 'device', 'api'));
DROP TABLE oauth_codes;
DROP TABLE oauth_clients;
