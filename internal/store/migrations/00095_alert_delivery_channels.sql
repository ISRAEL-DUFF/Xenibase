-- +goose Up
-- Alert delivery per channel: each event (firing, resolved) of an alert is
-- recorded once per channel (webhook, email), with that channel's own
-- attempts and backoff, so a broken channel is retried alone and the
-- working one is not sent the same event again. alerts.notified_at and
-- resolved_notified_at still mark the event delivered on every channel.
CREATE TABLE alert_deliveries (
  alert_id uuid NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
  event    text NOT NULL CHECK (event IN ('firing', 'resolved')),
  channel  text NOT NULL CHECK (channel IN ('webhook', 'email')),
  sent_at  timestamptz,
  attempts int NOT NULL DEFAULT 0,
  error    text,
  next_at  timestamptz,
  PRIMARY KEY (alert_id, event, channel)
);

-- Hysteresis: a firing alert resolves only after several clear
-- evaluations in a row (from any server), counted here; a holding
-- condition resets them.
ALTER TABLE alerts ADD COLUMN clear_count int NOT NULL DEFAULT 0,
  ADD COLUMN cleared_since timestamptz;

-- Resolved alerts are pruned after 90 days.
CREATE INDEX alerts_resolved_at ON alerts (resolved_at) WHERE status = 'resolved';

-- Grace periods (a pooler, the waker or a pooler host failing for a
-- minute before it is down), shared by every server instead of kept in
-- each one's memory. seen_at is when a server last saw the key failing:
-- a row nobody refreshed for a while starts over.
CREATE TABLE alert_grace (
  key     text PRIMARY KEY,
  since   timestamptz NOT NULL DEFAULT now(),
  seen_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE alert_grace;
DROP INDEX alerts_resolved_at;
ALTER TABLE alerts DROP COLUMN cleared_since, DROP COLUMN clear_count;
DROP TABLE alert_deliveries;
