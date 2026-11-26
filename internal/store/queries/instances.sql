
-- name: SetInstancePITRDays :exec
-- tenant: system - a dedicated project's instance the request already authorized.
UPDATE instances SET pitr_days = @pitr_days WHERE id = @id;

-- name: SetInstanceSize :exec
-- tenant: system - a dedicated project's instance the request already authorized.
UPDATE instances SET cpu_limit = @cpu_limit, mem_limit_mb = @mem_limit_mb, volume_gb = @volume_gb, profile = sqlc.narg(profile),
  max_connections = @max_connections
WHERE id = @id;
