-- name: CreateRead :one
WITH created AS (
    INSERT INTO reads (case_id, kind, reader_user_id)
    VALUES ($1, $2, $3)
    RETURNING id, case_id, kind, locked_at, created_at, reader_user_id
)
SELECT created.id, created.case_id, created.kind,
       created.locked_at, created.created_at, created.reader_user_id,
       users.display_name AS reader_display_name
FROM created
JOIN users ON users.id = created.reader_user_id;

-- name: GetRead :one
SELECT r.id, r.case_id, r.kind, r.locked_at, r.created_at, r.reader_user_id,
       u.display_name AS reader_display_name
FROM reads r
JOIN users u ON u.id = r.reader_user_id
WHERE r.case_id = $1 AND r.kind = $2;

-- name: GetReadForUpdate :one
SELECT r.id, r.case_id, r.kind, r.locked_at, r.created_at, r.reader_user_id,
       u.display_name AS reader_display_name
FROM reads r
JOIN users u ON u.id = r.reader_user_id
WHERE r.case_id = $1 AND r.kind = $2
FOR UPDATE OF r;

-- name: LockRead :one
WITH locked AS (
    UPDATE reads
    SET locked_at = now()
    WHERE reads.id = $1 AND reads.locked_at IS NULL
    RETURNING id, case_id, kind, locked_at, created_at, reader_user_id
)
SELECT locked.id, locked.case_id, locked.kind,
       locked.locked_at, locked.created_at, locked.reader_user_id,
       users.display_name AS reader_display_name
FROM locked
JOIN users ON users.id = locked.reader_user_id;
