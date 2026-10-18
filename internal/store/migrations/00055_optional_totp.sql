-- +goose Up
-- Two-factor sign-in is opt-in for ordinary accounts (platform staff still need
-- it): a signed-in user turns it on from their account page, which keeps the
-- authenticator they are enrolling in a challenge of its own kind.
ALTER TABLE auth_challenges DROP CONSTRAINT auth_challenges_kind_check;
ALTER TABLE auth_challenges ADD CONSTRAINT auth_challenges_kind_check
  CHECK (kind IN ('login', 'setup', 'enroll', 'enable_totp'));

-- +goose Down
DELETE FROM auth_challenges WHERE kind = 'enable_totp';
ALTER TABLE auth_challenges DROP CONSTRAINT auth_challenges_kind_check;
ALTER TABLE auth_challenges ADD CONSTRAINT auth_challenges_kind_check
  CHECK (kind IN ('login', 'setup', 'enroll'));
