-- name: CreateCase :one
INSERT INTO cases (source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid, status, finalized_at, created_at;

-- name: GetCase :one
SELECT id, source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid, status, finalized_at, created_at
FROM cases
WHERE id = $1;

-- name: GetCaseBySeriesUID :one
SELECT id, source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid, status, finalized_at, created_at
FROM cases
WHERE series_instance_uid = $1;

-- name: ListCasesByReader :many
-- A reader's own cases. Before an initial read exists the importer is the
-- owner, so a case confirmed by this user still appears; once any reader has
-- started the initial read, only that reader sees it. The projection matches
-- GetCase so the transport allowlist stays identical.
SELECT c.id, c.source, c.study_instance_uid, c.series_instance_uid, c.orthanc_series_id, c.frame_of_reference_uid, c.status, c.finalized_at, c.created_at
FROM cases c
WHERE EXISTS (
    SELECT 1 FROM reads r
    WHERE r.case_id = c.id AND r.kind = 'initial' AND r.reader_user_id = @user_id
)
OR (
    NOT EXISTS (
        SELECT 1 FROM reads r WHERE r.case_id = c.id AND r.kind = 'initial'
    )
    AND EXISTS (
        SELECT 1 FROM events e
        WHERE e.case_id = c.id AND e.action = 'case_imported' AND e.actor_user_id = @user_id
    )
)
ORDER BY c.created_at DESC, c.id DESC
LIMIT @row_limit;

-- name: GetCaseForUpdate :one
SELECT id, source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid, status, finalized_at, created_at
FROM cases
WHERE id = $1
FOR UPDATE;

-- name: UpdateCaseState :one
UPDATE cases
SET status = $2, finalized_at = sqlc.narg('finalized_at')
WHERE id = $1
RETURNING id, source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid, status, finalized_at, created_at;

-- name: FinalizeCase :one
UPDATE cases
SET status = 'finalized', finalized_at = now()
WHERE id = $1 AND status = 'initial_locked' AND finalized_at IS NULL
RETURNING id, source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid, status, finalized_at, created_at;
