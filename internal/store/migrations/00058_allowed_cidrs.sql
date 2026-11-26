-- +goose Up
-- A project's IP allowlist: when not empty, the poolers accept its
-- customer logins only from these networks (canonical CIDRs, at most 50).
-- Xenibase's own logins (the edge's, the SLA probe's) aren't limited.
ALTER TABLE projects ADD COLUMN allowed_cidrs text[] NOT NULL DEFAULT '{}'
  CHECK (cardinality(allowed_cidrs) <= 50);

-- +goose Down
ALTER TABLE projects DROP COLUMN allowed_cidrs;
