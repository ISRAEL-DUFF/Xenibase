-- +goose Up
-- The reaper ends a statement that caught its cancel (V2 §10.4). PL/pgSQL
-- can trap query_canceled, so a cancelled backend can keep running the same
-- statement; the reaper records which backend and statement it cancelled
-- (pid, and the statement's query_start), and on a later pass terminates a
-- backend it finds still running it, recorded as statement_terminated.
ALTER TABLE reaped_sessions ADD COLUMN pid int, ADD COLUMN started_at timestamptz;
ALTER TABLE reaped_sessions DROP CONSTRAINT reaped_sessions_kind_check;
ALTER TABLE reaped_sessions ADD CONSTRAINT reaped_sessions_kind_check
  CHECK (kind IN ('statement', 'statement_terminated', 'idle_in_transaction'));

-- +goose Down
DELETE FROM reaped_sessions WHERE kind = 'statement_terminated';
ALTER TABLE reaped_sessions DROP CONSTRAINT reaped_sessions_kind_check;
ALTER TABLE reaped_sessions ADD CONSTRAINT reaped_sessions_kind_check
  CHECK (kind IN ('statement', 'idle_in_transaction'));
ALTER TABLE reaped_sessions DROP COLUMN started_at, DROP COLUMN pid;
