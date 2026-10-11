-- name: CreateMonitor :one
INSERT INTO monitors (
    project_id, name, kind, target, interval_seconds, timeout_seconds, retries,
    failure_threshold, expected_status, keyword, ssl_expiry_days, heartbeat_token, enabled
) VALUES (
    @project_id, @name, @kind, @target, @interval_seconds, @timeout_seconds, @retries,
    @failure_threshold, @expected_status, @keyword, @ssl_expiry_days, @heartbeat_token, @enabled
)
RETURNING *;

-- name: UpdateMonitor :one
UPDATE monitors
SET project_id = @project_id,
    name = @name,
    target = @target,
    interval_seconds = @interval_seconds,
    timeout_seconds = @timeout_seconds,
    retries = @retries,
    failure_threshold = @failure_threshold,
    expected_status = @expected_status,
    keyword = @keyword,
    ssl_expiry_days = @ssl_expiry_days,
    enabled = @enabled,
    updated_at = now()
WHERE monitors.id = @id
  AND project_id IN (SELECT p.id FROM projects p WHERE p.user_id = @user_id)
RETURNING *;

-- name: GetMonitor :one
SELECT * FROM monitors WHERE id = @id;

-- name: GetMonitorForUser :one
SELECT m.*
FROM monitors m
JOIN projects p ON p.id = m.project_id
WHERE m.id = @id AND p.user_id = @user_id;

-- name: ListMonitorsByUser :many
SELECT m.*, p.name AS project_name, p.slug AS project_slug
FROM monitors m
JOIN projects p ON p.id = m.project_id
WHERE p.user_id = @user_id
ORDER BY p.name, m.name, m.id;

-- name: ListMonitorsByProject :many
SELECT * FROM monitors WHERE project_id = @project_id ORDER BY name, id;

-- name: ListEnabledMonitors :many
SELECT * FROM monitors WHERE enabled ORDER BY id;

-- name: CountMonitorsByUser :one
SELECT count(*)
FROM monitors m
JOIN projects p ON p.id = m.project_id
WHERE p.user_id = @user_id;

-- name: DeleteMonitorForUser :execrows
DELETE FROM monitors
WHERE monitors.id = @id
  AND project_id IN (SELECT p.id FROM projects p WHERE p.user_id = @user_id);

-- name: SetMonitorEnabled :execrows
UPDATE monitors
SET enabled = @enabled, updated_at = now()
WHERE monitors.id = @id
  AND project_id IN (SELECT p.id FROM projects p WHERE p.user_id = @user_id);

-- name: UpdateMonitorState :exec
UPDATE monitors
SET status = @status,
    consecutive_failures = @consecutive_failures,
    failing_since = @failing_since,
    last_checked_at = @last_checked_at
WHERE id = @id;

-- name: GetMonitorByHeartbeatToken :one
SELECT * FROM monitors WHERE heartbeat_token = @heartbeat_token;

-- name: TouchHeartbeat :exec
UPDATE monitors SET last_heartbeat_at = @last_heartbeat_at WHERE id = @id;
