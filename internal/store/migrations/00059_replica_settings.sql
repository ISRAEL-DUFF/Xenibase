-- +goose Up
-- Per-replica standby settings (Taskiem's dedicated request): whether the
-- replica tells the primary which rows its queries still need
-- (hot_standby_feedback), and how long replay waits for a conflicting
-- query before cancelling it (max_standby_streaming_delay and
-- max_standby_archive_delay). Postgres's defaults: off, 30 s.
ALTER TABLE read_replicas
  ADD COLUMN hot_standby_feedback boolean NOT NULL DEFAULT false,
  ADD COLUMN max_standby_delay_s int NOT NULL DEFAULT 30 CHECK (max_standby_delay_s BETWEEN 0 AND 900),
  -- When the settings were last confirmed on the replica, and why the last
  -- attempt failed (cleared on success).
  ADD COLUMN settings_applied_at timestamptz,
  ADD COLUMN settings_error text;

-- +goose Down
ALTER TABLE read_replicas DROP COLUMN hot_standby_feedback, DROP COLUMN max_standby_delay_s,
  DROP COLUMN settings_applied_at, DROP COLUMN settings_error;
