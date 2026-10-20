package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/finalread"
	"nodule-reader/backend/internal/application/initialread"
	"nodule-reader/backend/internal/infrastructure/postgres"
)

// finalStore builds the LC1-13 runtime-role adapter on a fresh disposable
// database.
func finalStore(t *testing.T, h *storageHarness) (*postgres.FinalReadStore, *pgxpool.Pool) {
	t.Helper()
	pool := importRuntimePool(t, h)
	return postgres.NewFinalReadStore(pool), pool
}

// seedLockedCase creates a case whose independent initial read is locked and
// whose editable final read already exists, mirroring a committed submission.
func seedLockedCase(t *testing.T, pool *pgxpool.Pool, seriesUID string) (caseID, initialID, finalID int64) {
	t.Helper()
	ctx := context.Background()
	caseID = seedCase(t, pool, seriesUID, "awaiting_read")
	initialID = seedInitialRead(t, pool, caseID)
	if _, err := pool.Exec(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1`, initialID); err != nil {
		t.Fatalf("lock initial read: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO reads (case_id, kind, reader_user_id) VALUES ($1, 'final', $2) RETURNING id`,
		caseID, readerUserID(t, pool)).Scan(&finalID); err != nil {
		t.Fatalf("seed final read: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'initial_locked' WHERE id = $1`, caseID); err != nil {
		t.Fatalf("advance case: %v", err)
	}
	return caseID, initialID, finalID
}

// seedJob inserts one AI job in the requested state. A result document is only
// meaningful for a succeeded job.
func seedJob(t *testing.T, pool *pgxpool.Pool, caseID int64, status string, result []byte, errorCode *string) int64 {
	t.Helper()
	ctx := context.Background()
	var resultArg, codeArg, finishedArg any
	if result != nil {
		resultArg = result
	}
	if errorCode != nil {
		codeArg = *errorCode
	}
	if status == "succeeded" || status == "failed" {
		finishedArg = time.Now()
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO ai_jobs (case_id, status, result, error_code, finished_at) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		caseID, status, resultArg, codeArg, finishedArg).Scan(&id); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return id
}

// jobFingerprint renders one ai_jobs row as canonical text so a read that
// mutates job persistence is detectable.
func jobFingerprint(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var fingerprint string
	if err := pool.QueryRow(context.Background(),
		`SELECT status || '|' || COALESCE(result::text, '') || '|' || COALESCE(error_code, '') || '|' ||
		        COALESCE(started_at::text, '') || '|' || COALESCE(finished_at::text, '') || '|' ||
		        COALESCE(stderr_tail, '')
		 FROM ai_jobs WHERE id = $1`, id).Scan(&fingerprint); err != nil {
		t.Fatalf("job fingerprint: %v", err)
	}
	return fingerprint
}

// syntheticAISuccess builds a synthetic LC1-12 success document that satisfies
// the pinned contract. It is contract fixture data only and never evidence of
// real inference.
func syntheticAISuccess(t *testing.T, seriesUID, frameUID string, candidateIDs ...string) []byte {
	t.Helper()
	candidates := make([]map[string]any, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		candidates = append(candidates, map[string]any{
			"id":        id,
			"center_mm": []float64{-96.25, 39.05, -180.5},
			"size_mm":   []float64{11.2, 9.4, 8.0},
			"score":     0.94,
			"unknown":   "dropped",
		})
	}
	document := map[string]any{
		"schema_version":         "1",
		"series_instance_uid":    seriesUID,
		"frame_of_reference_uid": frameUID,
		"coordinate_system":      "DICOM_LPS_mm",
		"model": map[string]any{
			"bundle":            "lung_nodule_ct_detection",
			"bundle_version":    "0.6.11",
			"monai_version":     "1.4.0",
			"torch_version":     "2.7.0",
			"affine_lps_to_ras": true,
			"score_threshold":   0.5,
		},
		"input": map[string]any{
			"num_slices":  133,
			"spacing_mm":  []float64{0.7, 0.7, 1.0},
			"size_voxels": []int{512, 512, 133},
		},
		"candidates": candidates,
		"timing_s":   map[string]any{"load": 1.5, "inference": 2.5, "total": 4.0},
		"resources":  map[string]any{"device": "cuda", "peak_gpu_mem_mb": 1, "peak_rss_mb": 2},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal synthetic result: %v", err)
	}
	return raw
}

func countDecisions(t *testing.T, conn *pgx.Conn, readID int64) int {
	t.Helper()
	var count int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM candidate_decisions WHERE read_id = $1`, readID).Scan(&count); err != nil {
		t.Fatalf("decision count: %v", err)
	}
	return count
}

func readDecisions(t *testing.T, conn *pgx.Conn, readID int64) []finalread.Decision {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT candidate_id, decision, dismiss_reason FROM candidate_decisions WHERE read_id = $1 ORDER BY candidate_id`, readID)
	if err != nil {
		t.Fatalf("list decisions: %v", err)
	}
	defer rows.Close()
	decisions := []finalread.Decision{}
	for rows.Next() {
		var decision finalread.Decision
		if err := rows.Scan(&decision.CandidateID, &decision.Decision, &decision.DismissReason); err != nil {
			t.Fatalf("scan decision: %v", err)
		}
		decisions = append(decisions, decision)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("decision rows: %v", err)
	}
	return decisions
}

// TestFinalReadDisclosure proves the pre-lock gate: nothing AI or final may be
// read while the independent read is not locked, even when a succeeded job
// exists, and a locked-looking case without a real locked initial read is a
// conflict rather than a partial disclosure.
func TestFinalReadDisclosure(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := finalStore(t, h)
	initial := postgres.NewInitialReadStore(pool)

	t.Run("awaiting case is forbidden even with a succeeded job", func(t *testing.T) {
		caseID := seedCase(t, pool, "1.2.3.4.5.5101", "awaiting_read")
		seedInitialRead(t, pool, caseID)
		seedJob(t, pool, caseID, "succeeded", syntheticAISuccess(t, "1.2.3.4.5.5101", "1.2.3.4.7", "c1"), nil)

		if _, err := store.GetAI(ctx, caseID, readerUserID(t, pool)); !errors.Is(err, finalread.ErrForbidden) {
			t.Fatalf("GetAI error = %v, want ErrForbidden", err)
		}
		if _, err := store.GetFinal(ctx, caseID, readerUserID(t, pool)); !errors.Is(err, finalread.ErrForbidden) {
			t.Fatalf("GetFinal error = %v, want ErrForbidden", err)
		}
	})

	t.Run("locked case discloses AI and the copied final read", func(t *testing.T) {
		caseID := seedCase(t, pool, "1.2.3.4.5.5102", "awaiting_read")
		seedJob(t, pool, caseID, "succeeded", syntheticAISuccess(t, "1.2.3.4.5.5102", "1.2.3.4.7", "c1", "c2"), nil)

		if _, err := initial.ReplaceInitialMarks(ctx, caseID, readerUserID(t, pool), []initialread.Mark{markA()}); err != nil {
			t.Fatalf("ReplaceInitialMarks: %v", err)
		}
		result, err := initial.SubmitInitial(ctx, caseID, readerUserID(t, pool))
		if err != nil {
			t.Fatalf("SubmitInitial: %v", err)
		}

		final, err := store.GetFinal(ctx, caseID, readerUserID(t, pool))
		if err != nil {
			t.Fatalf("GetFinal: %v", err)
		}
		if final.ID != result.FinalReadID || final.Kind != "final" || final.Reader != integrationReaderName {
			t.Fatalf("final identity = %+v", final)
		}
		assertMarksEqual(t, final.Marks, []initialread.Mark{markA()}, "final copy")
		if final.CandidateDecisions == nil || len(final.CandidateDecisions) != 0 {
			t.Fatalf("final decisions = %#v, want empty", final.CandidateDecisions)
		}

		ai, err := store.GetAI(ctx, caseID, readerUserID(t, pool))
		if err != nil {
			t.Fatalf("GetAI: %v", err)
		}
		if ai.Status != finalread.AISucceeded || len(ai.Candidates) != 2 || ai.Candidates[0].ID != "c1" {
			t.Fatalf("ai = %+v", ai)
		}
	})

	t.Run("locked status with an unlocked initial read conflicts", func(t *testing.T) {
		caseID := seedCase(t, pool, "1.2.3.4.5.5103", "initial_locked")
		seedInitialRead(t, pool, caseID)
		seedJob(t, pool, caseID, "queued", nil, nil)

		if _, err := store.GetAI(ctx, caseID, readerUserID(t, pool)); !errors.Is(err, finalread.ErrConflict) {
			t.Fatalf("GetAI error = %v, want ErrConflict", err)
		}
		if _, err := store.GetFinal(ctx, caseID, readerUserID(t, pool)); !errors.Is(err, finalread.ErrConflict) {
			t.Fatalf("GetFinal error = %v, want ErrConflict", err)
		}
	})

	t.Run("locked status without an initial read conflicts", func(t *testing.T) {
		caseID := seedCase(t, pool, "1.2.3.4.5.5104", "initial_locked")
		seedJob(t, pool, caseID, "queued", nil, nil)

		if _, err := store.GetAI(ctx, caseID, readerUserID(t, pool)); !errors.Is(err, finalread.ErrConflict) {
			t.Fatalf("GetAI error = %v, want ErrConflict", err)
		}
	})

	t.Run("missing final read conflicts", func(t *testing.T) {
		caseID := seedCase(t, pool, "1.2.3.4.5.5105", "initial_locked")
		initialID := seedInitialRead(t, pool, caseID)
		if _, err := pool.Exec(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1`, initialID); err != nil {
			t.Fatalf("lock initial: %v", err)
		}
		seedJob(t, pool, caseID, "queued", nil, nil)

		if _, err := store.GetFinal(ctx, caseID, readerUserID(t, pool)); !errors.Is(err, finalread.ErrConflict) {
			t.Fatalf("GetFinal error = %v, want ErrConflict", err)
		}
		// A locked case may still read its AI status without a final read.
		if ai, err := store.GetAI(ctx, caseID, readerUserID(t, pool)); err != nil || ai.Status != finalread.AIQueued {
			t.Fatalf("GetAI = %+v, %v", ai, err)
		}
	})

	t.Run("nonexistent case is not found", func(t *testing.T) {
		if _, err := store.GetAI(ctx, 999999, readerUserID(t, pool)); !errors.Is(err, finalread.ErrNotFound) {
			t.Fatalf("GetAI error = %v, want ErrNotFound", err)
		}
	})
}

// TestFinalReadAIProjection proves the state-specific AI projection, the latest
// job selection, corrupt-result handling and the failed-code allowlist.
func TestFinalReadAIProjection(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := finalStore(t, h)

	series := func(index int) string { return "1.2.3.4.5.52" + strconv.Itoa(index) }

	t.Run("queued and running carry no candidates", func(t *testing.T) {
		for index, status := range []string{"queued", "running"} {
			caseID, _, _ := seedLockedCase(t, pool, series(10+index))
			seedJob(t, pool, caseID, status, nil, nil)

			ai, err := store.GetAI(ctx, caseID, readerUserID(t, pool))
			if err != nil {
				t.Fatalf("%s: %v", status, err)
			}
			if ai.Status != status || ai.Candidates != nil || ai.Error != nil || ai.Model != nil {
				t.Fatalf("%s projection = %+v", status, ai)
			}
		}
	})

	t.Run("failed projects the fixed message and allowlisted code", func(t *testing.T) {
		known := "AI_TIMEOUT"
		unknown := "SUPER_PRIVATE_FAILURE"

		cases := []struct {
			name     string
			code     *string
			wantCode string
		}{
			{"known", &known, "AI_TIMEOUT"},
			{"unknown", &unknown, finalread.AIUnavailableCode},
			{"null", nil, finalread.AIUnavailableCode},
		}
		for index, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				caseID, _, _ := seedLockedCase(t, pool, series(20+index))
				seedJob(t, pool, caseID, "failed", nil, test.code)

				ai, err := store.GetAI(ctx, caseID, readerUserID(t, pool))
				if err != nil {
					t.Fatalf("GetAI: %v", err)
				}
				if ai.Status != finalread.AIFailed || ai.Error == nil {
					t.Fatalf("ai = %+v", ai)
				}
				if ai.Error.Code != test.wantCode || ai.Error.Message != finalread.AIUnavailableMessage {
					t.Fatalf("error = %+v, want code %q", ai.Error, test.wantCode)
				}
				if ai.Candidates == nil || len(ai.Candidates) != 0 {
					t.Fatalf("candidates = %#v, want empty", ai.Candidates)
				}
			})
		}
	})

	t.Run("succeeded projects candidates without unknown metadata", func(t *testing.T) {
		caseID, _, _ := seedLockedCase(t, pool, series(30))
		seedJob(t, pool, caseID, "succeeded", syntheticAISuccess(t, series(30), "1.2.3.4.7", "c1", "c2"), nil)

		ai, err := store.GetAI(ctx, caseID, readerUserID(t, pool))
		if err != nil {
			t.Fatalf("GetAI: %v", err)
		}
		if ai.Status != finalread.AISucceeded || len(ai.Candidates) != 2 {
			t.Fatalf("ai = %+v", ai)
		}
		candidate := ai.Candidates[0]
		if candidate.ID != "c1" || candidate.Score != 0.94 ||
			len(candidate.CenterMM) != 3 || candidate.CenterMM[0] != -96.25 ||
			len(candidate.SizeMM) != 3 || candidate.SizeMM[0] != 11.2 {
			t.Fatalf("candidate = %+v", candidate)
		}
		if ai.Model == nil || ai.Model.Bundle != "lung_nodule_ct_detection" || ai.Model.BundleVersion != "0.6.11" || ai.Model.ScoreThreshold != 0.5 {
			t.Fatalf("model = %+v", ai.Model)
		}
		if ai.Input == nil || ai.Input.NumSlices != 133 || len(ai.Input.SizeVoxels) != 3 {
			t.Fatalf("input = %+v", ai.Input)
		}
		if ai.Timing == nil || ai.Timing.Total != 4.0 {
			t.Fatalf("timing = %+v", ai.Timing)
		}
		if ai.Resources == nil || ai.Resources.Device != "cuda" {
			t.Fatalf("resources = %+v", ai.Resources)
		}
		if ai.Error != nil {
			t.Fatalf("succeeded error = %+v, want nil", ai.Error)
		}
	})

	t.Run("latest job wins deterministically", func(t *testing.T) {
		caseID, _, _ := seedLockedCase(t, pool, series(31))
		seedJob(t, pool, caseID, "succeeded", syntheticAISuccess(t, series(31), "1.2.3.4.7", "old"), nil)
		seedJob(t, pool, caseID, "failed", nil, nil)

		ai, err := store.GetAI(ctx, caseID, readerUserID(t, pool))
		if err != nil {
			t.Fatalf("GetAI: %v", err)
		}
		if ai.Status != finalread.AIFailed || ai.Candidates == nil || len(ai.Candidates) != 0 {
			t.Fatalf("latest = %+v, want the failed job", ai)
		}
	})

	t.Run("corrupt success is a dependency failure", func(t *testing.T) {
		caseID, _, _ := seedLockedCase(t, pool, series(32))
		seedJob(t, pool, caseID, "succeeded", []byte(`{"schema_version":"1"}`), nil)

		if _, err := store.GetAI(ctx, caseID, readerUserID(t, pool)); !errors.Is(err, finalread.ErrDependency) {
			t.Fatalf("GetAI error = %v, want ErrDependency", err)
		}
	})

	t.Run("linkage and coordinate mutations are dependency failures", func(t *testing.T) {
		mutations := []struct {
			name   string
			mutate func(map[string]any)
		}{
			{"coordinate_system", func(document map[string]any) { document["coordinate_system"] = "RAS_mm" }},
			{"series_instance_uid", func(document map[string]any) { document["series_instance_uid"] = "1.2.3.4.5.5299" }},
			{"frame_of_reference_uid", func(document map[string]any) { document["frame_of_reference_uid"] = "1.2.3.4.8" }},
		}
		for index, mutation := range mutations {
			t.Run(mutation.name, func(t *testing.T) {
				uid := series(70 + index)
				caseID, _, _ := seedLockedCase(t, pool, uid)

				var document map[string]any
				if err := json.Unmarshal(syntheticAISuccess(t, uid, "1.2.3.4.7", "c1"), &document); err != nil {
					t.Fatalf("decode fixture: %v", err)
				}
				mutation.mutate(document)
				raw, err := json.Marshal(document)
				if err != nil {
					t.Fatalf("encode fixture: %v", err)
				}
				jobID := seedJob(t, pool, caseID, "succeeded", raw, nil)

				clinicalBefore := snapshotClinical(t, pool)
				jobBefore := jobFingerprint(t, pool, jobID)

				if _, err := store.GetAI(ctx, caseID, readerUserID(t, pool)); !errors.Is(err, finalread.ErrDependency) {
					t.Fatalf("GetAI error = %v, want ErrDependency", err)
				}

				if clinicalAfter := snapshotClinical(t, pool); clinicalBefore != clinicalAfter {
					t.Fatalf("clinical state changed by a rejected read:\nbefore %s\nafter  %s", clinicalBefore, clinicalAfter)
				}
				if jobAfter := jobFingerprint(t, pool, jobID); jobBefore != jobAfter {
					t.Fatalf("job row changed by a rejected read: before %s after %s", jobBefore, jobAfter)
				}
			})
		}
	})
}

// TestFinalReadReplacement proves the three-origin replacement, explicit
// acceptance, candidate membership, clearing, reload and rollback of a failed
// replacement.
func TestFinalReadReplacement(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := finalStore(t, h)
	service := finalread.NewService(store)

	series := func(index int) string { return "1.2.3.4.5.53" + strconv.Itoa(index) }

	seed := func(index int, candidateIDs ...string) (int64, int64) {
		caseID, _, finalID := seedLockedCase(t, pool, series(index))
		seedJob(t, pool, caseID, "succeeded", syntheticAISuccess(t, series(index), "1.2.3.4.7", candidateIDs...), nil)
		return caseID, finalID
	}

	t.Run("three origins with explicit acceptance and dismissal", func(t *testing.T) {
		caseID, finalID := seed(1, "c1", "c2")
		reason := "vessel, not a nodule"
		value := finalread.Replacement{
			Marks: []initialread.Mark{
				withFinalOrigin(markA(), finalread.OriginReader, nil),
				withFinalOrigin(markB(), finalread.OriginReaderAfterAI, nil),
				withFinalOrigin(markC(), finalread.OriginFromAICandidate, new("c1")),
			},
			CandidateDecisions: []finalread.Decision{
				{CandidateID: "c1", Decision: finalread.DecisionAccepted},
				{CandidateID: "c2", Decision: finalread.DecisionDismissed, DismissReason: &reason},
			},
		}
		saved, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), value)
		if err != nil {
			t.Fatalf("ReplaceFinal: %v", err)
		}
		if len(saved.Marks) != 3 || len(saved.CandidateDecisions) != 2 {
			t.Fatalf("saved = %+v", saved)
		}

		runtime := h.runtime(t)
		assertMarksEqual(t, readMarks(t, runtime, finalID), value.Marks, "stored marks")
		stored := readDecisions(t, runtime, finalID)
		if len(stored) != 2 || stored[0].CandidateID != "c1" || stored[1].DismissReason == nil || *stored[1].DismissReason != reason {
			t.Fatalf("stored decisions = %+v", stored)
		}
		if countEvents(t, runtime, caseID, "final_read_saved") != 1 {
			t.Fatalf("final_read_saved event count wrong")
		}

		reloaded, err := store.GetFinal(ctx, caseID, readerUserID(t, pool))
		if err != nil {
			t.Fatalf("GetFinal: %v", err)
		}
		assertMarksEqual(t, reloaded.Marks, value.Marks, "reloaded marks")
	})

	t.Run("clearing the whole read", func(t *testing.T) {
		caseID, finalID := seed(2, "c1")
		if _, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), finalread.Replacement{
			Marks:              []initialread.Mark{withFinalOrigin(markA(), finalread.OriginFromAICandidate, new("c1"))},
			CandidateDecisions: []finalread.Decision{{CandidateID: "c1", Decision: finalread.DecisionAccepted}},
		}); err != nil {
			t.Fatalf("first ReplaceFinal: %v", err)
		}
		saved, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), finalread.Replacement{
			Marks:              []initialread.Mark{},
			CandidateDecisions: []finalread.Decision{},
		})
		if err != nil {
			t.Fatalf("clear ReplaceFinal: %v", err)
		}
		if len(saved.Marks) != 0 || len(saved.CandidateDecisions) != 0 {
			t.Fatalf("cleared = %+v", saved)
		}
		runtime := h.runtime(t)
		if countDecisions(t, runtime, finalID) != 0 {
			t.Fatalf("decisions not cleared")
		}
	})

	t.Run("unknown and cross-case candidates rejected", func(t *testing.T) {
		caseID, _ := seed(3, "c1")
		// Another case defines c9; it must not be a member of this case.
		otherCase, _, _ := seedLockedCase(t, pool, series(99))
		seedJob(t, pool, otherCase, "succeeded", syntheticAISuccess(t, series(99), "1.2.3.4.7", "c9"), nil)

		for name, value := range map[string]finalread.Replacement{
			"unknown candidate decision": {
				Marks:              []initialread.Mark{},
				CandidateDecisions: []finalread.Decision{{CandidateID: "cX", Decision: finalread.DecisionAccepted}},
			},
			"cross-case candidate decision": {
				Marks:              []initialread.Mark{},
				CandidateDecisions: []finalread.Decision{{CandidateID: "c9", Decision: finalread.DecisionAccepted}},
			},
			"cross-case candidate mark": {
				Marks:              []initialread.Mark{withFinalOrigin(markA(), finalread.OriginFromAICandidate, new("c9"))},
				CandidateDecisions: []finalread.Decision{{CandidateID: "c9", Decision: finalread.DecisionAccepted}},
			},
		} {
			if _, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), value); !errors.Is(err, finalread.ErrInvalid) {
				t.Fatalf("%s: error = %v, want ErrInvalid", name, err)
			}
		}
	})

	t.Run("candidate references rejected without a succeeded result", func(t *testing.T) {
		value := finalread.Replacement{
			Marks:              []initialread.Mark{},
			CandidateDecisions: []finalread.Decision{{CandidateID: "c1", Decision: finalread.DecisionAccepted}},
		}
		for index, status := range []string{"queued", "running", "failed"} {
			caseID, _, _ := seedLockedCase(t, pool, series(40+index))
			var code *string
			seedJob(t, pool, caseID, status, nil, code)
			if _, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), value); !errors.Is(err, finalread.ErrInvalid) {
				t.Fatalf("%s: error = %v, want ErrInvalid", status, err)
			}
		}
	})

	t.Run("physician-only replacement survives unavailable and corrupt AI", func(t *testing.T) {
		value := finalread.Replacement{
			Marks:              []initialread.Mark{withFinalOrigin(markA(), finalread.OriginReaderAfterAI, nil)},
			CandidateDecisions: []finalread.Decision{},
		}
		failedCase, _, _ := seedLockedCase(t, pool, series(50))
		seedJob(t, pool, failedCase, "failed", nil, nil)
		if _, err := service.ReplaceFinal(ctx, failedCase, readerUserID(t, pool), value); err != nil {
			t.Fatalf("failed job: %v", err)
		}

		corruptCase, _, _ := seedLockedCase(t, pool, series(51))
		seedJob(t, pool, corruptCase, "succeeded", []byte(`{"schema_version":"1"}`), nil)
		if _, err := service.ReplaceFinal(ctx, corruptCase, readerUserID(t, pool), value); err != nil {
			t.Fatalf("corrupt result: %v", err)
		}
	})

	t.Run("failed replacement leaves the saved snapshot intact", func(t *testing.T) {
		caseID, finalID := seed(60, "c1")
		committed := finalread.Replacement{
			Marks:              []initialread.Mark{withFinalOrigin(markA(), finalread.OriginFromAICandidate, new("c1"))},
			CandidateDecisions: []finalread.Decision{{CandidateID: "c1", Decision: finalread.DecisionAccepted}},
		}
		if _, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), committed); err != nil {
			t.Fatalf("commit: %v", err)
		}
		runtime := h.runtime(t)
		eventsBefore := countEvents(t, runtime, caseID, "final_read_saved")

		cleanup := installSubmitFailure(t, h, "lc1_13_mark",
			`CREATE TRIGGER %FUNC% BEFORE INSERT ON marks FOR EACH ROW WHEN (NEW.origin = 'from_ai_candidate') EXECUTE FUNCTION %FUNC%()`,
			`RAISE EXCEPTION 'lc1-13 mark fixture';`)
		defer cleanup()

		attempt := finalread.Replacement{
			Marks: []initialread.Mark{
				withFinalOrigin(markB(), finalread.OriginFromAICandidate, new("c1")),
				withFinalOrigin(markC(), finalread.OriginReader, nil),
			},
			CandidateDecisions: []finalread.Decision{{CandidateID: "c1", Decision: finalread.DecisionAccepted}},
		}
		if _, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), attempt); !errors.Is(err, finalread.ErrDependency) {
			t.Fatalf("error = %v, want ErrDependency", err)
		}

		assertMarksEqual(t, readMarks(t, runtime, finalID), committed.Marks, "marks after rollback")
		if got := readDecisions(t, runtime, finalID); len(got) != 1 || got[0].CandidateID != "c1" {
			t.Fatalf("decisions after rollback = %+v", got)
		}
		if countEvents(t, runtime, caseID, "final_read_saved") != eventsBefore {
			t.Fatalf("event survived the failed replacement")
		}
	})

	t.Run("audit failure rolls the whole replacement back", func(t *testing.T) {
		caseID, finalID := seed(61, "c1")
		runtime := h.runtime(t)

		cleanup := installSubmitFailure(t, h, "lc1_13_event",
			`CREATE TRIGGER %FUNC% BEFORE INSERT ON events FOR EACH ROW WHEN (NEW.action = 'final_read_saved') EXECUTE FUNCTION %FUNC%()`,
			`RAISE EXCEPTION 'lc1-13 event fixture';`)
		defer cleanup()

		value := finalread.Replacement{
			Marks:              []initialread.Mark{withFinalOrigin(markA(), finalread.OriginReader, nil)},
			CandidateDecisions: []finalread.Decision{},
		}
		if _, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), value); !errors.Is(err, finalread.ErrDependency) {
			t.Fatalf("error = %v, want ErrDependency", err)
		}
		if countMarksOn(t, runtime, finalID) != 0 {
			t.Fatalf("marks survived the failed audit append")
		}
		if countEvents(t, runtime, caseID, "final_read_saved") != 0 {
			t.Fatalf("event count wrong after rollback")
		}
	})
}

// withFinalOrigin sets a mark's origin and candidate linkage for the final read.
func withFinalOrigin(mark initialread.Mark, origin string, candidateID *string) initialread.Mark {
	mark.Origin = origin
	mark.AICandidateID = candidateID
	return mark
}

// TestFinalReadCanceledUnderLock proves a replacement that cannot acquire the
// case lock records nothing and leaves the pool usable.
func TestFinalReadCanceledUnderLock(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := finalStore(t, h)
	service := finalread.NewService(store)

	caseID, _, finalID := seedLockedCase(t, pool, "1.2.3.4.5.5401")
	seedJob(t, pool, caseID, "succeeded", syntheticAISuccess(t, "1.2.3.4.5.5401", "1.2.3.4.7", "c1"), nil)

	holder := h.runtime(t)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM cases WHERE id = $1 FOR UPDATE`, caseID); err != nil {
		t.Fatalf("hold case lock: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	_, err = service.ReplaceFinal(waitCtx, caseID, readerUserID(t, pool), finalread.Replacement{
		Marks:              []initialread.Mark{withFinalOrigin(markA(), finalread.OriginReader, nil)},
		CandidateDecisions: []finalread.Decision{{CandidateID: "c1", Decision: finalread.DecisionAccepted}},
	})
	if !errors.Is(err, finalread.ErrDependency) {
		t.Fatalf("error = %v, want ErrDependency", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}

	runtime := h.runtime(t)
	if countMarksOn(t, runtime, finalID) != 0 || countDecisions(t, runtime, finalID) != 0 {
		t.Fatalf("canceled replacement persisted data")
	}
	if countEvents(t, runtime, caseID, "final_read_saved") != 0 {
		t.Fatalf("canceled replacement recorded an event")
	}
	// The pool stays usable after the canceled request.
	if _, err := store.GetFinal(ctx, caseID, readerUserID(t, pool)); err != nil {
		t.Fatalf("pool unusable after cancellation: %v", err)
	}
}
