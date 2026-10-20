-- name: AppendEvent :one
INSERT INTO events (case_id, actor, action, detail, actor_user_id)
VALUES (sqlc.narg('case_id'), $1, $2, $3, sqlc.narg('actor_user_id'))
RETURNING id, case_id, actor, action, detail, at, actor_user_id;

-- name: ListEvents :many
SELECT id, case_id, actor, action, detail, at, actor_user_id
FROM events
WHERE case_id = $1
ORDER BY id;
