-- +goose Up
-- A planned switchover in progress on an HA instance. The leader watcher
-- runs on one xenibase-server (under an advisory lock), but a switchover
-- runs wherever its operation does: the watcher leaves the instance to
-- the switchover while this is set and recent, rather than pause its
-- poolers or record the handover as a failover.
ALTER TABLE instances ADD COLUMN switchover_started_at timestamptz;

-- +goose Down
ALTER TABLE instances DROP COLUMN switchover_started_at;
