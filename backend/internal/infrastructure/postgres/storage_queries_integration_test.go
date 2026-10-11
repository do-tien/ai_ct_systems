package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"nodule-reader/backend/internal/infrastructure/postgres/lc1db"
)

func createCase(t *testing.T, q *lc1db.Queries, seriesUID string) lc1db.Case {
	t.Helper()
	row, err := q.CreateCase(context.Background(), lc1db.CreateCaseParams{
		Source:              "LIDC-IDRI/QA-SYNTHETIC",
		StudyInstanceUid:    "1.2.3.4.5",
		SeriesInstanceUid:   seriesUID,
		OrthancSeriesID:     "orthanc-1",
		FrameOfReferenceUid: "1.2.3.4.7",
	})
	if err != nil {
		t.Fatalf("CreateCase: %v", err)
	}
	return row
}

func createRead(t *testing.T, q *lc1db.Queries, caseID int64, kind string) lc1db.Read {
	t.Helper()
	row, err := q.CreateRead(context.Background(), lc1db.CreateReadParams{CaseID: caseID, Kind: kind, Reader: "dr-synthetic"})
	if err != nil {
		t.Fatalf("CreateRead: %v", err)
	}
	return row
}

func createMark(t *testing.T, q *lc1db.Queries, readID int64, origin string, candidate string, note string) lc1db.Mark {
	t.Helper()
	params := lc1db.CreateMarkParams{
		ReadID:          readID,
		P1Mm:            []float64{-102.3, 45.1, -180.5},
		P2Mm:            []float64{-90.2, 33.0, -180.5},
		CenterMm:        []float64{-96.25, 39.05, -180.5},
		DiameterMm:      17.0,
		Origin:          origin,
		ViewPlaneNormal: []float64{0, 0, 1},
	}
	if candidate != "" {
		params.AiCandidateID = pgtype.Text{String: candidate, Valid: true}
	}
	if note != "" {
		params.Note = pgtype.Text{String: note, Valid: true}
	}
	row, err := q.CreateMark(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateMark: %v", err)
	}
	return row
}

func TestLC1StorageQueries(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	owner := h.owner(t)
	ctx := context.Background()

	t.Run("rollback_persists_nothing", func(t *testing.T) {
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		q := lc1db.New(tx)
		row := createCase(t, q, "1.2.3.4.20")
		read := createRead(t, q, row.ID, "initial")
		createMark(t, q, read.ID, "reader", "", "draft")
		if _, err := q.CreateAIJob(ctx, row.ID); err != nil {
			t.Fatalf("CreateAIJob: %v", err)
		}
		if _, err := q.AppendEvent(ctx, lc1db.AppendEventParams{Actor: "dr-synthetic", Action: "case_viewed", CaseID: pgtype.Int8{Int64: row.ID, Valid: true}}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}

		q = lc1db.New(owner)
		if _, err := q.GetCase(ctx, row.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetCase after rollback error = %v, want ErrNoRows", err)
		}
	})

	t.Run("commit_persists", func(t *testing.T) {
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		q := lc1db.New(tx)
		row := createCase(t, q, "1.2.3.4.21")
		read := createRead(t, q, row.ID, "initial")
		mark := createMark(t, q, read.ID, "reader", "", "kept")
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		q = lc1db.New(owner)
		got, err := q.GetCase(ctx, row.ID)
		if err != nil {
			t.Fatalf("GetCase: %v", err)
		}
		if got.SeriesInstanceUid != "1.2.3.4.21" || got.Status != "awaiting_read" {
			t.Fatalf("case = %+v", got)
		}
		marks, err := q.ListMarks(ctx, read.ID)
		if err != nil {
			t.Fatalf("ListMarks: %v", err)
		}
		if len(marks) != 1 || marks[0].ID != mark.ID {
			t.Fatalf("marks = %+v", marks)
		}
	})

	t.Run("copy_marks_keeps_geometry_with_new_ids", func(t *testing.T) {
		tx, _ := owner.Begin(ctx)
		defer tx.Rollback(ctx)
		q := lc1db.New(tx)
		row := createCase(t, q, "1.2.3.4.22")
		initial := createRead(t, q, row.ID, "initial")
		source := createMark(t, q, initial.ID, "from_ai_candidate", "c1", "note-1")
		final := createRead(t, q, row.ID, "final")

		if err := q.CopyMarks(ctx, lc1db.CopyMarksParams{SourceReadID: initial.ID, TargetReadID: final.ID}); err != nil {
			t.Fatalf("CopyMarks: %v", err)
		}
		copied, err := q.ListMarks(ctx, final.ID)
		if err != nil {
			t.Fatalf("ListMarks: %v", err)
		}
		if len(copied) != 1 {
			t.Fatalf("copied %d marks, want 1", len(copied))
		}
		got := copied[0]
		if got.ID == source.ID {
			t.Fatalf("copied mark reused id %d", got.ID)
		}
		if !reflect.DeepEqual(got.P1Mm, source.P1Mm) || !reflect.DeepEqual(got.CenterMm, source.CenterMm) ||
			got.DiameterMm != source.DiameterMm || got.Origin != source.Origin ||
			got.AiCandidateID != source.AiCandidateID || got.Note != source.Note {
			t.Fatalf("copied mark = %+v, source = %+v", got, source)
		}
	})

	t.Run("invalid_mark_rolls_back_replacement", func(t *testing.T) {
		tx, _ := owner.Begin(ctx)
		q := lc1db.New(tx)
		row := createCase(t, q, "1.2.3.4.23")
		read := createRead(t, q, row.ID, "initial")
		createMark(t, q, read.ID, "reader", "", "original")
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit seed: %v", err)
		}

		replace, err := owner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin replace: %v", err)
		}
		rq := lc1db.New(replace)
		if err := rq.DeleteMarks(ctx, read.ID); err != nil {
			t.Fatalf("DeleteMarks: %v", err)
		}
		createMark(t, rq, read.ID, "reader", "", "replacement")
		_, err = rq.CreateMark(ctx, lc1db.CreateMarkParams{
			ReadID:     read.ID,
			P1Mm:       []float64{0, 0, 0},
			P2Mm:       []float64{1, 1, 1},
			CenterMm:   []float64{0.5, 0.5, 0.5},
			DiameterMm: 0,
			Origin:     "reader",
		})
		if err == nil {
			t.Fatalf("invalid mark unexpectedly succeeded")
		}
		if err := replace.Rollback(ctx); err != nil {
			t.Fatalf("rollback replace: %v", err)
		}
		marks, err := lc1db.New(owner).ListMarks(ctx, read.ID)
		if err != nil {
			t.Fatalf("ListMarks: %v", err)
		}
		if len(marks) != 1 || marks[0].Note.String != "original" {
			t.Fatalf("marks after rollback = %+v", marks)
		}
	})

	t.Run("job_result_jsonb_round_trip", func(t *testing.T) {
		tx, _ := owner.Begin(ctx)
		defer tx.Rollback(ctx)
		q := lc1db.New(tx)
		row := createCase(t, q, "1.2.3.4.24")
		job, err := q.CreateAIJob(ctx, row.ID)
		if err != nil {
			t.Fatalf("CreateAIJob: %v", err)
		}
		if _, err := q.StartAIJob(ctx, job.ID); err != nil {
			t.Fatalf("StartAIJob: %v", err)
		}

		document := map[string]any{
			"schema_version": "1",
			"model":          map[string]any{"bundle": "lung_nodule_ct_detection", "bundle_version": "0.6.11", "score_threshold": 0.5},
			"input":          map[string]any{"num_slices": 133, "spacing_mm": []any{0.7, 0.7, 2.0}, "size_voxels": []any{512, 512, 133}},
			"candidates":     []any{map[string]any{"id": "c1", "center_mm": []any{-1.5, 2.25, 3.0}, "size_mm": []any{4.0, 4.0, 4.0}, "score": 0.94}},
			"timing_s":       map[string]any{"load": 1.5, "inference": 2.5, "total": 4.0},
			"resources":      map[string]any{"device": "cpu", "peak_rss_mb": 1234.5},
			"extra_metadata": map[string]any{"note": "synthetic", "revision": "abc123"},
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		done, err := q.CompleteAIJob(ctx, lc1db.CompleteAIJobParams{ID: job.ID, Result: encoded})
		if err != nil {
			t.Fatalf("CompleteAIJob: %v", err)
		}
		if done.Status != "succeeded" {
			t.Fatalf("status = %s", done.Status)
		}

		loaded, err := q.GetAIJob(ctx, job.ID)
		if err != nil {
			t.Fatalf("GetAIJob: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(loaded.Result, &decoded); err != nil {
			t.Fatalf("unmarshal stored result: %v", err)
		}
		var expected map[string]any
		if err := json.Unmarshal(encoded, &expected); err != nil {
			t.Fatalf("unmarshal expected result: %v", err)
		}
		if !reflect.DeepEqual(decoded, expected) {
			t.Fatalf("round-trip mismatch:\n got %#v\nwant %#v", decoded, expected)
		}

		// A succeeded job is terminal: a second terminal transition is rejected
		// by the conditional predicate rather than overwriting the result.
		if _, err := q.FailAIJob(ctx, lc1db.FailAIJobParams{ID: job.ID, ErrorCode: pgtype.Text{String: "MODEL_OOM", Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("FailAIJob on succeeded job error = %v, want pgx.ErrNoRows", err)
		}

		// A separate running job can still be failed legally.
		second, err := q.CreateAIJob(ctx, row.ID)
		if err != nil {
			t.Fatalf("CreateAIJob 2: %v", err)
		}
		if _, err := q.StartAIJob(ctx, second.ID); err != nil {
			t.Fatalf("StartAIJob 2: %v", err)
		}
		failed, err := q.FailAIJob(ctx, lc1db.FailAIJobParams{ID: second.ID, ErrorCode: pgtype.Text{String: "MODEL_OOM", Valid: true}})
		if err != nil {
			t.Fatalf("FailAIJob: %v", err)
		}
		if failed.Status != "failed" || failed.Result != nil {
			t.Fatalf("failed job = status %s result %v", failed.Status, failed.Result)
		}
	})

	t.Run("latest_job_is_highest_id", func(t *testing.T) {
		tx, _ := owner.Begin(ctx)
		defer tx.Rollback(ctx)
		q := lc1db.New(tx)
		row := createCase(t, q, "1.2.3.4.25")
		if _, err := q.CreateAIJob(ctx, row.ID); err != nil {
			t.Fatalf("CreateAIJob 1: %v", err)
		}
		second, err := q.CreateAIJob(ctx, row.ID)
		if err != nil {
			t.Fatalf("CreateAIJob 2: %v", err)
		}
		latest, err := q.GetLatestAIJob(ctx, row.ID)
		if err != nil {
			t.Fatalf("GetLatestAIJob: %v", err)
		}
		if latest.ID != second.ID {
			t.Fatalf("latest = %d, want %d", latest.ID, second.ID)
		}
		// Neither job operation changes case status or marks.
		got, err := q.GetCase(ctx, row.ID)
		if err != nil {
			t.Fatalf("GetCase: %v", err)
		}
		if got.Status != "awaiting_read" {
			t.Fatalf("case status = %s", got.Status)
		}
	})

	t.Run("decision_upsert_and_isolation", func(t *testing.T) {
		tx, _ := owner.Begin(ctx)
		defer tx.Rollback(ctx)
		q := lc1db.New(tx)
		row := createCase(t, q, "1.2.3.4.26")
		readA := createRead(t, q, row.ID, "initial")
		readB := createRead(t, q, row.ID, "final")

		dismissed, err := q.UpsertCandidateDecision(ctx, lc1db.UpsertCandidateDecisionParams{
			ReadID: readA.ID, CandidateID: "c1", Decision: "dismissed",
			DismissReason: pgtype.Text{String: "vessel", Valid: true},
		})
		if err != nil {
			t.Fatalf("upsert dismissed: %v", err)
		}
		if !dismissed.DismissReason.Valid || dismissed.DismissReason.String != "vessel" {
			t.Fatalf("dismissed reason = %+v", dismissed.DismissReason)
		}

		accepted, err := q.UpsertCandidateDecision(ctx, lc1db.UpsertCandidateDecisionParams{
			ReadID: readA.ID, CandidateID: "c1", Decision: "accepted",
		})
		if err != nil {
			t.Fatalf("upsert accepted: %v", err)
		}
		if accepted.DismissReason.Valid {
			t.Fatalf("accepted kept reason %+v", accepted.DismissReason)
		}

		dismissedAgain, err := q.UpsertCandidateDecision(ctx, lc1db.UpsertCandidateDecisionParams{
			ReadID: readA.ID, CandidateID: "c1", Decision: "dismissed",
			DismissReason: pgtype.Text{String: "not a nodule", Valid: true},
		})
		if err != nil {
			t.Fatalf("upsert dismissed again: %v", err)
		}
		if !dismissedAgain.DismissReason.Valid || dismissedAgain.DismissReason.String != "not a nodule" {
			t.Fatalf("re-dismissed reason = %+v", dismissedAgain.DismissReason)
		}

		other, err := q.ListCandidateDecisions(ctx, readB.ID)
		if err != nil {
			t.Fatalf("ListCandidateDecisions B: %v", err)
		}
		if len(other) != 0 {
			t.Fatalf("read B decisions = %+v, want none", other)
		}
	})

	t.Run("no_row_lookup", func(t *testing.T) {
		q := lc1db.New(owner)
		if _, err := q.GetCase(ctx, 987654321); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetCase missing error = %v, want ErrNoRows", err)
		}
		if _, err := q.GetRead(ctx, lc1db.GetReadParams{CaseID: 987654321, Kind: "initial"}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetRead missing error = %v, want ErrNoRows", err)
		}
	})

	t.Run("canceled_context_writes_nothing", func(t *testing.T) {
		conn := h.owner(t)
		q := lc1db.New(conn)
		var before int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM cases`).Scan(&before); err != nil {
			t.Fatalf("count before: %v", err)
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := q.CreateCase(canceled, lc1db.CreateCaseParams{
			Source: "s", StudyInstanceUid: "1.1", SeriesInstanceUid: "1.2.3.4.27",
			OrthancSeriesID: "o", FrameOfReferenceUid: "1.1",
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled CreateCase error = %v, want context.Canceled", err)
		}
		var after int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM cases`).Scan(&after); err != nil {
			t.Fatalf("count after: %v", err)
		}
		if after != before {
			t.Fatalf("cases changed from %d to %d after a canceled write", before, after)
		}
	})

	t.Run("query_after_rollback_fails", func(t *testing.T) {
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		q := lc1db.New(tx)
		row := createCase(t, q, "1.2.3.4.28")
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if _, err := q.GetCase(ctx, row.ID); err == nil {
			t.Fatalf("query on rolled-back transaction unexpectedly succeeded")
		}
	})
}
