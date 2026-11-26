-- +goose Up
-- A region's own API domain: its projects' backend services are at
-- https://<ref>.<api_domain>, served by the region's edge, so a Lagos
-- project's API traffic stays in Lagos. Empty is XENIBASE_API_DOMAIN.
ALTER TABLE regions ADD COLUMN api_domain text NOT NULL DEFAULT ''
  CHECK (api_domain = '' OR api_domain ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$');
CREATE UNIQUE INDEX regions_api_domain ON regions (api_domain) WHERE api_domain <> '';

-- Early access: the organisations that may create projects in a hidden
-- region, for a rehearsal or a first customer before the region opens.
CREATE TABLE region_early_access (
  region_id  text NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  org_id     uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (region_id, org_id)
);
CREATE INDEX region_early_access_org ON region_early_access (org_id);

-- +goose Down
DROP TABLE region_early_access;
DROP INDEX regions_api_domain;
ALTER TABLE regions DROP COLUMN api_domain;
