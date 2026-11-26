-- +goose Up
-- A token created by another token records it as its parent, and revoking
-- a token revokes its children (and theirs) in the same statement: a
-- token disconnected, disabled with its OAuth app, or revoked by hand
-- can't leave behind the tokens it made for itself. Tokens made by tokens
-- before this migration have no recorded parent.
ALTER TABLE api_tokens ADD COLUMN parent_id uuid REFERENCES api_tokens(id) ON DELETE CASCADE;
CREATE INDEX api_tokens_parent ON api_tokens (parent_id) WHERE parent_id IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION api_tokens_revoke_children() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE api_tokens SET revoked_at = NEW.revoked_at, revoked_by = NEW.revoked_by
  WHERE parent_id = NEW.id AND revoked_at IS NULL;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE TRIGGER api_tokens_revoke_children AFTER UPDATE OF revoked_at ON api_tokens
  FOR EACH ROW WHEN (OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL)
  EXECUTE FUNCTION api_tokens_revoke_children();

-- +goose Down
DROP TRIGGER api_tokens_revoke_children ON api_tokens;
DROP FUNCTION api_tokens_revoke_children();
DROP INDEX api_tokens_parent;
ALTER TABLE api_tokens DROP COLUMN parent_id;
