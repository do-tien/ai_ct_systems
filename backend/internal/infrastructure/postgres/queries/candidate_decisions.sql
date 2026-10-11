-- name: ListCandidateDecisions :many
SELECT read_id, candidate_id, decision, dismiss_reason, decided_at
FROM candidate_decisions
WHERE read_id = $1
ORDER BY candidate_id;

-- name: UpsertCandidateDecision :one
INSERT INTO candidate_decisions (read_id, candidate_id, decision, dismiss_reason)
VALUES ($1, $2, $3, $4)
ON CONFLICT (read_id, candidate_id) DO UPDATE
SET decision = EXCLUDED.decision, dismiss_reason = EXCLUDED.dismiss_reason, decided_at = now()
RETURNING read_id, candidate_id, decision, dismiss_reason, decided_at;

-- name: DeleteCandidateDecisions :exec
DELETE FROM candidate_decisions
WHERE read_id = $1;
