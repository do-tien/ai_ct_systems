-- name: ListMarks :many
SELECT id, read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, ai_candidate_id, view_plane_normal, note
FROM marks
WHERE read_id = $1
ORDER BY id;

-- name: CreateMark :one
INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, ai_candidate_id, view_plane_normal, note)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, ai_candidate_id, view_plane_normal, note;

-- name: DeleteMarks :exec
DELETE FROM marks
WHERE read_id = $1;

-- name: CopyMarks :exec
INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, ai_candidate_id, view_plane_normal, note)
SELECT sqlc.arg('target_read_id'), m.p1_mm, m.p2_mm, m.center_mm, m.diameter_mm, m.origin, m.ai_candidate_id, m.view_plane_normal, m.note
FROM marks m
WHERE m.read_id = sqlc.arg('source_read_id')
ORDER BY m.id;
