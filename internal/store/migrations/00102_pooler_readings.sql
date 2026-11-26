-- +goose Up
-- The metrics collector runs on one xenibase-server at a time. Each server
-- still reads the PgBouncers next to it (XENIBASE_POOLER_LOCAL), which only
-- it administers, and leaves its readings here for the collecting server:
-- one row per server process and pooler, with each database's clients,
-- waiting clients and transaction counter.
CREATE TABLE pooler_readings (
  source     text PRIMARY KEY,            -- "<server process id>/<pooler>/<address>"
  pools      jsonb NOT NULL,              -- {"<database>": {"c": clients, "w": waiting, "x": transactions}}
  sampled_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE pooler_readings;
