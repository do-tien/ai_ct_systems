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
