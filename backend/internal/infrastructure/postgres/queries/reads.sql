-- name: CreateRead :one
INSERT INTO reads (case_id, kind, reader)
VALUES ($1, $2, $3)
RETURNING id, case_id, kind, reader, locked_at, created_at;

-- name: GetRead :one
SELECT id, case_id, kind, reader, locked_at, created_at
FROM reads
WHERE case_id = $1 AND kind = $2;

-- name: GetReadForUpdate :one
SELECT id, case_id, kind, reader, locked_at, created_at
FROM reads
WHERE case_id = $1 AND kind = $2
FOR UPDATE;

-- name: LockRead :one
UPDATE reads
SET locked_at = now()
WHERE id = $1 AND locked_at IS NULL
RETURNING id, case_id, kind, reader, locked_at, created_at;
