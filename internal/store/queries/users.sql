-- name: CreateUser :one
INSERT INTO users (email, password_hash, plan)
VALUES (@email, @password_hash, @plan)
RETURNING *;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = @email;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = @id;

-- name: SetUserPlan :exec
UPDATE users SET plan = @plan WHERE id = @id;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, expires_at)
VALUES (@token_hash, @user_id, @expires_at);

-- name: GetSessionUser :one
SELECT u.id, u.email, u.password_hash, u.plan, u.created_at
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = @token_hash AND s.expires_at > now();

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = @token_hash;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= now();
