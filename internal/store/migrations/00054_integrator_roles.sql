-- +goose Up
-- NOLOGIN roles an operator created on a dedicated project's instance for
-- an integrator (Taskiem's taskiem_app and taskiem_dispatch): the demotion
-- check, moves and the isolation audit expect them instead of flagging
-- them as custom roles.
ALTER TABLE projects ADD COLUMN integrator_roles text[];

-- +goose Down
ALTER TABLE projects DROP COLUMN integrator_roles;
