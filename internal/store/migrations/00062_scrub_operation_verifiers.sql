-- +goose Up
-- Password rotations kept the owner's new and previous SCRAM verifiers in
-- operations.params ("verifier", "previous_verifier"), and credential
-- switches the new owner's ("verifier"), outside "secrets", so they stayed
-- after the operation ended and the API returned them to anyone who could
-- view the project. They now live in params.secrets, which is removed when
-- an operation ends. Finished operations lose them; one still queued or
-- running has them moved into secrets.
UPDATE operations SET params = params - 'verifier' - 'previous_verifier'
WHERE kind IN ('rotate', 'switch_credentials') AND status IN ('succeeded', 'failed')
  AND (params ? 'verifier' OR params ? 'previous_verifier');

UPDATE operations
SET params = (params - 'verifier' - 'previous_verifier')
  || jsonb_build_object('secrets', coalesce(params->'secrets', '{}'::jsonb)
       || jsonb_strip_nulls(jsonb_build_object('verifier', params->'verifier', 'previous_verifier', params->'previous_verifier')))
WHERE kind IN ('rotate', 'switch_credentials') AND status IN ('queued', 'running')
  AND (params ? 'verifier' OR params ? 'previous_verifier');

-- +goose Down
-- The scrubbed verifiers are gone; queued and running operations get theirs
-- back where the previous release reads them.
UPDATE operations
SET params = params || jsonb_strip_nulls(jsonb_build_object(
  'verifier', params->'secrets'->'verifier', 'previous_verifier', params->'secrets'->'previous_verifier'))
WHERE kind IN ('rotate', 'switch_credentials') AND status IN ('queued', 'running') AND params ? 'secrets';
