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

-- What a project's cached image renders (files/<ref>/transforms/) take,
-- measured by the nightly reconciler: metered with its files.
ALTER TABLE project_storage ADD COLUMN transform_bytes bigint NOT NULL DEFAULT 0;

-- When the storage reconciler last ran (V4 §5.6): one row, so it runs
-- nightly on one server across restarts.
CREATE TABLE storage_reconciler (
  id          boolean PRIMARY KEY DEFAULT true CHECK (id),
  last_run_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE storage_reconciler;
ALTER TABLE project_storage DROP COLUMN transform_bytes;
DROP TABLE cdn_purges;
