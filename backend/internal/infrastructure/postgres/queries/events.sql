-- name: AppendEvent :one
INSERT INTO events (case_id, actor, action, detail)
VALUES (sqlc.narg('case_id'), $1, $2, $3)
RETURNING id, case_id, actor, action, detail, at;

-- name: ListEvents :many
SELECT id, case_id, actor, action, detail, at
FROM events
WHERE case_id = $1
ORDER BY id;
