-- Indexes for the retention sweep (internal/tenancy/retention.go): reaped
-- sessions by age, and finished operations whose parameters and step log
-- have not been pruned yet. Built concurrently, so a large operations table
-- takes writes while they are built.
-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS reaped_sessions_created ON reaped_sessions (created_at);
CREATE INDEX CONCURRENTLY IF NOT EXISTS operations_unpruned_finished ON operations (finished_at)
  WHERE status IN ('succeeded', 'failed') AND NOT (params @> '{"pruned": true}');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS operations_unpruned_finished;
DROP INDEX CONCURRENTLY IF EXISTS reaped_sessions_created;
