-- CDN purges waiting for the CDN (V4 §5.4).

-- name: EnqueueCDNPurge :one
-- tenant: system - a storage change's purge, kept until the CDN takes it.
INSERT INTO cdn_purges (project_id, prefixes) VALUES (@project_id, @prefixes) RETURNING id;

-- name: ClaimCDNPurges :many
-- tenant: system - the storage sweep: purges due again, held from other servers for five minutes.
UPDATE cdn_purges SET next_at = now() + interval '5 minutes'
WHERE id IN (SELECT id FROM cdn_purges WHERE next_at <= now() ORDER BY id LIMIT @max_rows FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: FinishCDNPurge :exec
-- tenant: system - a purge the CDN took.
DELETE FROM cdn_purges WHERE id = @id;

-- name: FailCDNPurge :exec
-- tenant: system - a purge the CDN refused: tried again after a minute, doubling to an hour.
UPDATE cdn_purges SET attempts = attempts + 1, last_error = @last_error,
  next_at = now() + least(interval '1 hour', interval '1 minute' * power(2, least(attempts, 6)))
WHERE id = @id;

-- name: DropStaleCDNPurges :execrows
-- tenant: system - purges older than the longest cache time (a year): nothing they were for is cached any more.
DELETE FROM cdn_purges WHERE created_at < now() - interval '366 days';
