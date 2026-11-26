-- +goose Up
-- A payment event that failed to process is processed again when the
-- provider retries it, when the re-query finds it, and by the hourly sweep
-- of failed events. claimed_at is when its processing last started (an
-- unfinished one older than a few minutes was cut off and is taken over);
-- attempts counts how often it was processed.
ALTER TABLE payment_events
  ADD COLUMN attempts   int NOT NULL DEFAULT 1,
  ADD COLUMN claimed_at timestamptz NOT NULL DEFAULT now();
UPDATE payment_events SET claimed_at = received_at;
CREATE INDEX payment_events_retry ON payment_events (provider, received_at)
  WHERE outcome = 'failed' OR outcome IS NULL;

-- +goose Down
DROP INDEX payment_events_retry;
ALTER TABLE payment_events DROP COLUMN claimed_at, DROP COLUMN attempts;
