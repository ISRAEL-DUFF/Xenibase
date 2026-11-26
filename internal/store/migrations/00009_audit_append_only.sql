-- The audit log is append-only (spec §7.5): the triggers refuse UPDATE,
-- DELETE and TRUNCATE from any role, so no query xenibase-server runs can
-- change or remove a row. They stop mistakes and SQL injection through the
-- server's queries, not a compromised server: xenibase-server connects as
-- the role that owns the table, which can DISABLE TRIGGER or drop it
-- (docs/security-review.md §7.5).
-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION audit_log_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'audit_log is append-only (% refused)', TG_OP USING ERRCODE = 'insufficient_privilege';
END $$;
-- +goose StatementEnd
CREATE TRIGGER audit_log_no_update_delete BEFORE UPDATE OR DELETE ON audit_log
  FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();

-- +goose Down
DROP TRIGGER audit_log_no_truncate ON audit_log;
DROP TRIGGER audit_log_no_update_delete ON audit_log;
DROP FUNCTION audit_log_append_only();
