-- name: OpenIncident :one
INSERT INTO incidents (monitor_id, started_at, cause)
VALUES (@monitor_id, @started_at, @cause)
RETURNING *;

-- name: GetOpenIncident :one
SELECT * FROM incidents WHERE monitor_id = @monitor_id AND resolved_at IS NULL;

-- name: ResolveIncident :one
UPDATE incidents
SET resolved_at = @resolved_at
WHERE monitor_id = @monitor_id AND resolved_at IS NULL
RETURNING *;

-- name: MarkIncidentAlerted :exec
UPDATE incidents
SET last_alert_at = @last_alert_at, alerts_sent = alerts_sent + 1
WHERE id = @id;

-- name: ListIncidentsForMonitorSince :many
SELECT * FROM incidents
WHERE monitor_id = @monitor_id
  AND (resolved_at IS NULL OR resolved_at >= @since::timestamptz)
ORDER BY started_at;

-- name: ListIncidentsForProjectSince :many
SELECT i.*, m.name AS monitor_name
FROM incidents i
JOIN monitors m ON m.id = i.monitor_id
WHERE m.project_id = @project_id
  AND (i.resolved_at IS NULL OR i.resolved_at >= @since::timestamptz)
ORDER BY i.started_at DESC;

-- name: ListIncidentsByUser :many
SELECT i.*, m.name AS monitor_name, p.name AS project_name
FROM incidents i
JOIN monitors m ON m.id = i.monitor_id
JOIN projects p ON p.id = m.project_id
WHERE p.user_id = @user_id
ORDER BY i.started_at DESC
LIMIT @max_rows;
