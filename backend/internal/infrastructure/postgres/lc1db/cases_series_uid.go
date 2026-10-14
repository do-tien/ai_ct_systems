// This file carries one hand-written query in the exact shape sqlc emits for
// queries/cases.sql. sqlc is unavailable in the offline build environment, so
// GetCaseBySeriesUID cannot be regenerated here. When sqlc is next available,
// delete this file and run `sqlc generate`: cases.sql.go will then define the
// same method, and leaving both would fail to compile (a loud, obvious failure).

package lc1db

import "context"

const getCaseBySeriesUID = `-- name: GetCaseBySeriesUID :one
SELECT id, source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid, status, finalized_at, created_at
FROM cases
WHERE series_instance_uid = $1
`

func (q *Queries) GetCaseBySeriesUID(ctx context.Context, seriesInstanceUid string) (Case, error) {
	row := q.db.QueryRow(ctx, getCaseBySeriesUID, seriesInstanceUid)
	var i Case
	err := row.Scan(
		&i.ID,
		&i.Source,
		&i.StudyInstanceUid,
		&i.SeriesInstanceUid,
		&i.OrthancSeriesID,
		&i.FrameOfReferenceUid,
		&i.Status,
		&i.FinalizedAt,
		&i.CreatedAt,
	)
	return i, err
}
