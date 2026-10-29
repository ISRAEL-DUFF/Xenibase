-- +goose Up
-- A job has at most one running run and one queued run (the overlap
-- policies: skip, or queue one). The scheduler and "Run now" on another
-- server could both start a run; dispatch now locks the job row, and these
-- indexes hold the rule. Duplicates left by that race are failed first,
-- keeping the newest.
UPDATE job_runs r SET status = 'failed', finished_at = now(), error = 'interrupted: the server stopped during the run'
WHERE r.status IN ('queued', 'running')
  AND EXISTS (SELECT 1 FROM job_runs k WHERE k.job_id = r.job_id AND k.status = r.status AND k.id > r.id);
CREATE UNIQUE INDEX job_runs_one_running ON job_runs (job_id) WHERE status = 'running';
CREATE UNIQUE INDEX job_runs_one_queued ON job_runs (job_id) WHERE status = 'queued';

-- +goose Down
DROP INDEX job_runs_one_queued;
DROP INDEX job_runs_one_running;
