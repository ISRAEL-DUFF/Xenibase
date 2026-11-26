-- +goose Up
-- The sign-in lockout doubles with each failure past the threshold, up to a
-- day, and emails the account's owner (at most once an hour,
-- users.lockout_notified_at). Addresses with no enabled account are counted
-- and locked the same way, so the lock does not tell an attacker which
-- addresses are registered. Rows are forgotten a day after the last failure.
ALTER TABLE users ADD COLUMN lockout_notified_at timestamptz;
CREATE TABLE login_address_failures (
  email         citext PRIMARY KEY,
  failed_logins int NOT NULL DEFAULT 0,
  locked_until  timestamptz,
  updated_at    timestamptz NOT NULL
);
CREATE INDEX login_address_failures_updated ON login_address_failures (updated_at);

-- +goose Down
DROP TABLE login_address_failures;
ALTER TABLE users DROP COLUMN lockout_notified_at;
