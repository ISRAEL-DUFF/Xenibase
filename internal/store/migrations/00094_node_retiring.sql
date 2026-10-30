-- +goose Up
-- The empty-node sweep takes a node out of placement before it deletes
-- the server: lifecycle 'retiring' (placement takes only 'active' nodes)
-- from retiring_at, with the lifecycle it had (retiring_from) to go back
-- to should something have been placed there after all. Only after a
-- grace period, with occupancy counted again under the node's lock, are
-- its clusters retired and its server deleted.
ALTER TABLE nodes DROP CONSTRAINT nodes_lifecycle_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_lifecycle_check
  CHECK (lifecycle IN ('active', 'provisioning', 'draining', 'retiring'));
ALTER TABLE nodes ADD COLUMN retiring_at timestamptz,
  ADD COLUMN retiring_from text CHECK (retiring_from IN ('active', 'draining'));

-- +goose Down
UPDATE nodes SET lifecycle = coalesce(retiring_from, 'active') WHERE lifecycle = 'retiring';
ALTER TABLE nodes DROP COLUMN retiring_from, DROP COLUMN retiring_at;
ALTER TABLE nodes DROP CONSTRAINT nodes_lifecycle_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_lifecycle_check
  CHECK (lifecycle IN ('active', 'provisioning', 'draining'));
