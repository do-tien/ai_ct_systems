-- name: CreateUser :one
INSERT INTO users (display_name, pin_lookup, pin_hash)
VALUES ($1, $2, $3)
RETURNING id, display_name, disabled_at, created_at, updated_at;

-- name: ListUsers :many
SELECT id, display_name, disabled_at, created_at, updated_at
FROM users
ORDER BY id;

-- name: FindActiveUserByPINLookup :one
SELECT id, display_name, pin_hash, created_at, updated_at
FROM users
WHERE pin_lookup = $1 AND disabled_at IS NULL;

-- name: RotateUserPIN :one
UPDATE users
SET pin_lookup = $2, pin_hash = $3, updated_at = now()
WHERE id = $1
RETURNING id, display_name, disabled_at, created_at, updated_at;

-- name: DisableUser :one
UPDATE users
SET disabled_at = now(), updated_at = now()
WHERE id = $1 AND disabled_at IS NULL
RETURNING id, display_name, disabled_at, created_at, updated_at;

-- name: CreateSession :exec
INSERT INTO auth_sessions (token_digest, user_id, expires_at)
VALUES ($1, $2, $3);

-- name: FindSession :one
SELECT s.token_digest, s.user_id, u.display_name, s.created_at, s.expires_at
FROM auth_sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_digest = $1 AND s.expires_at > $2 AND u.disabled_at IS NULL;

-- name: DeleteSession :exec
DELETE FROM auth_sessions
WHERE token_digest = $1;

-- name: DeleteSessionsForUser :exec
DELETE FROM auth_sessions
WHERE user_id = $1;

-- name: DeleteExpiredSessions :exec
DELETE FROM auth_sessions
WHERE expires_at <= $1;
