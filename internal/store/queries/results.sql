-- name: InsertCheckResult :exec
INSERT INTO check_results (monitor_id, checked_at, ok, latency_ms, status_code, message)
VALUES (@monitor_id, @checked_at, @ok, @latency_ms, @status_code, @message);

-- name: ListRecentResults :many
SELECT * FROM check_results
WHERE monitor_id = @monitor_id
ORDER BY checked_at DESC
LIMIT @max_rows;

-- name: DeleteResultsBefore :execrows
DELETE FROM check_results WHERE checked_at < @before;

-- name: LatencyStats :one
SELECT
    count(*)::bigint AS checks,
    count(*) FILTER (WHERE ok)::bigint AS ok_checks,
    COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE ok), 0)::float8 AS p50,
    COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE ok), 0)::float8 AS p95
FROM check_results
WHERE monitor_id = @monitor_id AND checked_at >= @since;

-- name: MonitorMetrics :many
SELECT
    m.id,
    m.name,
    m.kind,
    m.status,
    p.slug AS project_slug,
    COALESCE(s.checks, 0)::bigint AS checks,
    COALESCE(s.failures, 0)::bigint AS failures,
    COALESCE(s.p50, 0)::float8 AS p50,
    COALESCE(s.p95, 0)::float8 AS p95
FROM monitors m
JOIN projects p ON p.id = m.project_id
LEFT JOIN LATERAL (
    SELECT
        count(*) AS checks,
        count(*) FILTER (WHERE NOT r.ok) AS failures,
        percentile_cont(0.5) WITHIN GROUP (ORDER BY r.latency_ms) FILTER (WHERE r.ok) AS p50,
        percentile_cont(0.95) WITHIN GROUP (ORDER BY r.latency_ms) FILTER (WHERE r.ok) AS p95
    FROM check_results r
    WHERE r.monitor_id = m.id AND r.checked_at >= @since
) s ON true
WHERE m.enabled
ORDER BY m.id;
