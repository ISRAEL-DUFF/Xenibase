-- +goose Up
-- V4 §5.4: CDN purges of files that left a public bucket's URLs (a bucket
-- made private or deleted, an object deleted, replaced or moved), kept
-- until the CDN takes them: the storage sweep retries each with a backoff
-- of up to an hour.
CREATE TABLE cdn_purges (
  id         bigserial PRIMARY KEY,
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  prefixes   text[] NOT NULL,
  attempts   int NOT NULL DEFAULT 0,
  last_error text,
  next_at    timestamptz NOT NULL DEFAULT now(),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX cdn_purges_due ON cdn_purges (next_at);

-- +goose Down
DROP TABLE cdn_purges;
