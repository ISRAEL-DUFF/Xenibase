-- +goose Up
-- A dedicated instance's max_connections, which grows with its memory
-- (it was 100 for every size). Existing instances keep 100 until resized.
-- A restore's instance never has fewer than its source: Postgres won't
-- replay WAL from a server that allowed more connections.
ALTER TABLE instances ADD COLUMN max_connections int NOT NULL DEFAULT 100
  CHECK (max_connections BETWEEN 20 AND 5000);

-- +goose Down
ALTER TABLE instances DROP COLUMN max_connections;
