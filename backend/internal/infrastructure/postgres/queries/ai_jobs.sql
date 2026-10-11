-- name: ListQueuedAIJobs :many
SELECT id
FROM ai_jobs
WHERE status = 'queued'
ORDER BY id;

-- name: CreateAIJob :one
INSERT INTO ai_jobs (case_id, status)
VALUES ($1, 'queued')
RETURNING id, case_id, status, result, error_code, stderr_tail, started_at, finished_at;

-- name: GetAIJob :one
SELECT id, case_id, status, result, error_code, stderr_tail, started_at, finished_at
FROM ai_jobs
WHERE id = $1;

-- name: GetLatestAIJob :one
SELECT id, case_id, status, result, error_code, stderr_tail, started_at, finished_at
FROM ai_jobs
WHERE case_id = $1
ORDER BY id DESC
LIMIT 1;

-- name: GetLatestAIJobForShare :one
SELECT id, case_id, status, result, error_code, stderr_tail, started_at, finished_at
FROM ai_jobs
WHERE case_id = $1
ORDER BY id DESC
LIMIT 1
FOR SHARE;

-- name: StartAIJob :one
UPDATE ai_jobs
SET status = 'running', started_at = now(), result = NULL, error_code = NULL, stderr_tail = NULL, finished_at = NULL
WHERE id = $1 AND status = 'queued'
RETURNING id, case_id, status, result, error_code, stderr_tail, started_at, finished_at;

-- name: CompleteAIJob :one
UPDATE ai_jobs
SET status = 'succeeded', result = $2, finished_at = now(), error_code = NULL, stderr_tail = NULL
WHERE id = $1 AND status = 'running'
RETURNING id, case_id, status, result, error_code, stderr_tail, started_at, finished_at;

-- name: FailAIJob :one
UPDATE ai_jobs
SET status = 'failed', error_code = $2, stderr_tail = $3, finished_at = now(), result = NULL
WHERE id = $1 AND status = 'running'
RETURNING id, case_id, status, result, error_code, stderr_tail, started_at, finished_at;
