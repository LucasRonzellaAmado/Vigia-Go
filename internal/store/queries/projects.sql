-- name: CreateProject :one
INSERT INTO projects (user_id, name, slug, custom_domain, public)
VALUES (@user_id, @name, @slug, @custom_domain, @public)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: ListProjectsByUser :many
SELECT * FROM projects WHERE user_id = @user_id ORDER BY created_at, id;

-- name: GetProjectForUser :one
SELECT * FROM projects WHERE id = @id AND user_id = @user_id;

-- name: GetProjectBySlug :one
SELECT * FROM projects WHERE slug = @slug;

-- name: GetProjectByDomain :one
SELECT * FROM projects WHERE custom_domain = @custom_domain;

-- name: UpdateProject :one
UPDATE projects
SET name = @name, slug = @slug, custom_domain = @custom_domain, public = @public
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: DeleteProjectForUser :execrows
DELETE FROM projects WHERE id = @id AND user_id = @user_id;

-- name: CountProjectsByUser :one
SELECT count(*) FROM projects WHERE user_id = @user_id;
