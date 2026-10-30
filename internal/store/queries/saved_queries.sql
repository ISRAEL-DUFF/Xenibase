-- The SQL Editor's saved queries. Everyone who can use a project's
-- console sees their own queries and the shared ones.

-- name: ListSavedQueries :many
SELECT q.*, (f.user_id IS NOT NULL)::bool AS favorite
FROM saved_queries q
LEFT JOIN saved_query_favorites f ON f.query_id = q.id AND f.user_id = @user_id AND f.org_id = @org_id
WHERE q.org_id = @org_id AND q.project_id = @project_id AND (q.owner_id = @user_id OR q.visibility = 'shared')
ORDER BY q.updated_at DESC;

-- name: GetSavedQuery :one
SELECT q.*, (f.user_id IS NOT NULL)::bool AS favorite
FROM saved_queries q
LEFT JOIN saved_query_favorites f ON f.query_id = q.id AND f.user_id = @user_id AND f.org_id = @org_id
WHERE q.id = @id AND q.org_id = @org_id AND q.project_id = @project_id AND (q.owner_id = @user_id OR q.visibility = 'shared');

-- name: CountUserSavedQueries :one
SELECT count(*)::int FROM saved_queries WHERE org_id = @org_id AND project_id = @project_id AND owner_id = @owner_id;

-- name: InsertSavedQuery :one
INSERT INTO saved_queries (org_id, project_id, owner_id, name, sql, visibility)
VALUES (@org_id, @project_id, @owner_id, @name, @sql, @visibility)
RETURNING *;

-- name: UpdateSavedQuery :one
UPDATE saved_queries SET name = @name, sql = @sql, visibility = @visibility, updated_at = now()
WHERE id = @id AND org_id = @org_id AND project_id = @project_id AND owner_id = @owner_id
RETURNING *;

-- name: DeleteSavedQuery :execrows
DELETE FROM saved_queries WHERE id = @id AND org_id = @org_id AND project_id = @project_id;

-- name: SetSavedQueryFavorite :exec
INSERT INTO saved_query_favorites (query_id, user_id, org_id) VALUES (@query_id, @user_id, @org_id)
ON CONFLICT DO NOTHING;

-- name: UnsetSavedQueryFavorite :exec
DELETE FROM saved_query_favorites WHERE query_id = @query_id AND user_id = @user_id AND org_id = @org_id;

-- name: MoveProjectSavedQueryFavorites :exec
UPDATE saved_query_favorites f SET org_id = @new_org_id
WHERE f.org_id = @org_id AND f.query_id IN (SELECT q.id FROM saved_queries q WHERE q.project_id = @project_id);
