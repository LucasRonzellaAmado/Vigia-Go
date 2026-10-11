-- name: CreateAlertChannel :one
INSERT INTO alert_channels (project_id, kind, name, target, enabled)
VALUES (@project_id, @kind, @name, @target, @enabled)
RETURNING *;

-- name: ListAlertChannelsByUser :many
SELECT c.*, p.name AS project_name
FROM alert_channels c
JOIN projects p ON p.id = c.project_id
WHERE p.user_id = @user_id
ORDER BY p.name, c.name, c.id;

-- name: ListEnabledChannelsForProject :many
SELECT * FROM alert_channels WHERE project_id = @project_id AND enabled ORDER BY id;

-- name: GetAlertChannelForUser :one
SELECT c.*
FROM alert_channels c
JOIN projects p ON p.id = c.project_id
WHERE c.id = @id AND p.user_id = @user_id;

-- name: DeleteAlertChannelForUser :execrows
DELETE FROM alert_channels
WHERE alert_channels.id = @id
  AND project_id IN (SELECT p.id FROM projects p WHERE p.user_id = @user_id);
