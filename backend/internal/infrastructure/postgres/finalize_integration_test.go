package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"nodule-reader/backend/internal/application/aijob"
	"nodule-reader/backend/internal/application/finalread"
	"nodule-reader/backend/internal/application/initialread"
)

// caseFinalizedEvents returns every case_finalized audit detail of a case in
// insertion order.
func caseFinalizedEvents(t *testing.T, conn *pgx.Conn, caseID int64) []map[string]any {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT detail FROM events WHERE case_id = $1 AND action = 'case_finalized' ORDER BY id`, caseID)
	if err != nil {
		t.Fatalf("list finalize events: %v", err)
	}
	defer rows.Close()
	events := []map[string]any{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan finalize event: %v", err)
		}
		var detail map[string]any
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("decode finalize detail: %v", err)
		}
		events = append(events, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("finalize event rows: %v", err)
	}
	return events
}

// TestFinalize proves that a locked initial read and an existing unlocked final
// read finalize regardless of the real AI job state, that the committed response
// agrees with storage, that preconditions conflict, and that a fault anywhere in
// the transaction rolls the whole thing back.
func TestFinalize(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := finalStore(t, h)

	series := func(index int) string { return "1.2.3.4.5.55" + strconv.Itoa(index) }

	t.Run("every job state finalizes with the recorded status", func(t *testing.T) {
		statuses := []string{"queued", "running", "succeeded", "failed"}
		for index, status := range statuses {
			t.Run(status, func(t *testing.T) {
				caseID, initialID, finalID := seedLockedCase(t, pool, series(index))
				var result []byte
				if status == "succeeded" {
					result = syntheticAISuccess(t, series(index), "1.2.3.4.7", "c1")
				}
				seedJob(t, pool, caseID, status, result, nil)

				outcome, err := store.Finalize(ctx, caseID, readerUserID(t, pool))
				if err != nil {
					t.Fatalf("Finalize: %v", err)
				}
				if outcome.Status != "finalized" || outcome.AIStatus != status {
					t.Fatalf("outcome = %+v", outcome)
				}

				runtime := h.runtime(t)
				caseState, finalizedAt := caseStatus(t, runtime, caseID)
				if caseState != "finalized" || finalizedAt == nil || !finalizedAt.Equal(outcome.FinalizedAt) {
					t.Fatalf("case state = %q finalized_at = %v outcome %v", caseState, finalizedAt, outcome.FinalizedAt)
				}
				if locked := readLockedAt(t, runtime, finalID); locked == nil {
					t.Fatalf("final read was not locked")
				}
				if locked := readLockedAt(t, runtime, initialID); locked == nil {
					t.Fatalf("initial read lock disappeared")
				}
				events := caseFinalizedEvents(t, runtime, caseID)
				if len(events) != 1 {
					t.Fatalf("case_finalized events = %d, want 1", len(events))
				}
				if events[0]["ai_status"] != status || events[0]["final_read_id"].(float64) != float64(finalID) {
					t.Fatalf("event detail = %+v", events[0])
				}
			})
		}
	})

	t.Run("prelock, repeat and missing rows conflict", func(t *testing.T) {
		// Pre-lock.
		awaiting := seedCase(t, pool, series(10), "awaiting_read")
		seedInitialRead(t, pool, awaiting)
		seedJob(t, pool, awaiting, "queued", nil, nil)
		if _, err := store.Finalize(ctx, awaiting, readerUserID(t, pool)); !errors.Is(err, finalread.ErrConflict) {
			t.Fatalf("prelock error = %v, want ErrConflict", err)
		}

		// Missing final read.
		noFinal := seedCase(t, pool, series(11), "initial_locked")
		initialID := seedInitialRead(t, pool, noFinal)
		if _, err := pool.Exec(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1`, initialID); err != nil {
			t.Fatalf("lock initial: %v", err)
		}
		seedJob(t, pool, noFinal, "queued", nil, nil)
		if _, err := store.Finalize(ctx, noFinal, readerUserID(t, pool)); !errors.Is(err, finalread.ErrConflict) {
			t.Fatalf("missing final error = %v, want ErrConflict", err)
		}

		// Missing job.
		noJob, _, _ := seedLockedCase(t, pool, series(12))
		if _, err := store.Finalize(ctx, noJob, readerUserID(t, pool)); !errors.Is(err, finalread.ErrConflict) {
			t.Fatalf("missing job error = %v, want ErrConflict", err)
		}

		// Already-locked final read.
		lockedFinal, _, finalID := seedLockedCase(t, pool, series(13))
		seedJob(t, pool, lockedFinal, "queued", nil, nil)
		if _, err := pool.Exec(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1`, finalID); err != nil {
			t.Fatalf("lock final: %v", err)
		}
		if _, err := store.Finalize(ctx, lockedFinal, readerUserID(t, pool)); !errors.Is(err, finalread.ErrConflict) {
			t.Fatalf("locked final error = %v, want ErrConflict", err)
		}

		// Repeat finalize.
		repeat, _, _ := seedLockedCase(t, pool, series(14))
		seedJob(t, pool, repeat, "queued", nil, nil)
		if _, err := store.Finalize(ctx, repeat, readerUserID(t, pool)); err != nil {
			t.Fatalf("first finalize: %v", err)
		}
		if _, err := store.Finalize(ctx, repeat, readerUserID(t, pool)); !errors.Is(err, finalread.ErrConflict) {
			t.Fatalf("repeat error = %v, want ErrConflict", err)
		}
	})

	t.Run("fault injection rolls the finalize back", func(t *testing.T) {
		fixtures := []struct {
			name          string
			createTrigger string
		}{
			{"final_lock", `CREATE TRIGGER %FUNC% BEFORE UPDATE ON reads FOR EACH ROW WHEN (NEW.locked_at IS NOT NULL) EXECUTE FUNCTION %FUNC%()`},
			{"case_update", `CREATE TRIGGER %FUNC% BEFORE UPDATE ON cases FOR EACH ROW WHEN (NEW.status = 'finalized') EXECUTE FUNCTION %FUNC%()`},
			{"event_insert", `CREATE TRIGGER %FUNC% BEFORE INSERT ON events FOR EACH ROW WHEN (NEW.action = 'case_finalized') EXECUTE FUNCTION %FUNC%()`},
		}
		for index, fixture := range fixtures {
			t.Run(fixture.name, func(t *testing.T) {
				caseID, _, finalID := seedLockedCase(t, pool, series(20+index))
				seedJob(t, pool, caseID, "queued", nil, nil)

				cleanup := installSubmitFailure(t, h, "lc1_13_fin_"+fixture.name, fixture.createTrigger,
					`RAISE EXCEPTION 'lc1-13 finalize fixture';`)
				defer cleanup()

				if _, err := store.Finalize(ctx, caseID, readerUserID(t, pool)); !errors.Is(err, finalread.ErrDependency) {
					t.Fatalf("error = %v, want ErrDependency", err)
				}

				runtime := h.runtime(t)
				caseState, finalizedAt := caseStatus(t, runtime, caseID)
				if caseState != "initial_locked" || finalizedAt != nil {
					t.Fatalf("case state after rollback = %q finalized_at = %v", caseState, finalizedAt)
				}
				if locked := readLockedAt(t, runtime, finalID); locked != nil {
					t.Fatalf("final read stayed locked after rollback")
				}
				if len(caseFinalizedEvents(t, runtime, caseID)) != 0 {
					t.Fatalf("event survived the failed finalize")
				}
			})
		}
	})
}

// TestFinalizeConcurrency proves two finalizes serialize to one winner, that a
// final PUT racing finalize leaves only a complete saved-before-lock version or a
// rejected write, and that a concurrent GET never observes mixed marks and
// decisions.
func TestFinalizeConcurrency(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := finalStore(t, h)
	service := finalread.NewService(store)

	t.Run("two finalizes yield one winner and one event", func(t *testing.T) {
		caseID, _, _ := seedLockedCase(t, pool, "1.2.3.4.5.5601")
		seedJob(t, pool, caseID, "queued", nil, nil)

		results := make([]error, 2)
		var wait sync.WaitGroup
		start := make(chan struct{})
		for index := 0; index < 2; index++ {
			wait.Add(1)
			go func(slot int) {
				defer wait.Done()
				<-start
				_, results[slot] = store.Finalize(ctx, caseID, readerUserID(t, pool))
			}(index)
		}
		close(start)
		wait.Wait()

		successes, conflicts := 0, 0
		for _, err := range results {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, finalread.ErrConflict) || errors.Is(err, finalread.ErrLocked):
				conflicts++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("successes = %d conflicts = %d, want 1/1", successes, conflicts)
		}
		if len(caseFinalizedEvents(t, h.runtime(t), caseID)) != 1 {
			t.Fatalf("event count wrong")
		}
	})

	t.Run("final PUT racing finalize stays coherent", func(t *testing.T) {
		caseID, _, finalID := seedLockedCase(t, pool, "1.2.3.4.5.5602")
		seedJob(t, pool, caseID, "succeeded", syntheticAISuccess(t, "1.2.3.4.5.5602", "1.2.3.4.7", "c1"), nil)

		// One complete generation is committed before the race, so a rejected
		// PUT must leave exactly this version rather than an empty read.
		prior := finalread.Replacement{
			Marks: []initialread.Mark{{
				P1MM: []float64{1, 1, 1}, P2MM: []float64{2, 2, 2}, CenterMM: []float64{1.5, 1.5, 1.5},
				DiameterMM: 1, Origin: finalread.OriginReader, ViewPlaneNormal: []float64{0, 0, 1}, Note: new("prior"),
			}},
			CandidateDecisions: []finalread.Decision{{CandidateID: "c1", Decision: finalread.DecisionAccepted}},
		}
		if _, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), prior); err != nil {
			t.Fatalf("seed prior generation: %v", err)
		}
		racing := finalread.Replacement{
			Marks: []initialread.Mark{{
				P1MM: []float64{3, 3, 3}, P2MM: []float64{4, 4, 4}, CenterMM: []float64{3.5, 3.5, 3.5},
				DiameterMM: 2, Origin: finalread.OriginReaderAfterAI, ViewPlaneNormal: []float64{0, 0, 1}, Note: new("racing"),
			}},
			CandidateDecisions: []finalread.Decision{{CandidateID: "c1", Decision: finalread.DecisionDismissed, DismissReason: new("vessel")}},
		}

		holder := h.runtime(t)
		holderPID := backendPID(t, holder)
		tx, err := holder.Begin(ctx)
		if err != nil {
			t.Fatalf("begin holder: %v", err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT id FROM cases WHERE id = $1 FOR UPDATE`, caseID); err != nil {
			t.Fatalf("hold case lock: %v", err)
		}

		var finalizeErr, putErr error
		var wait sync.WaitGroup
		start := make(chan struct{})
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			_, finalizeErr = store.Finalize(ctx, caseID, readerUserID(t, pool))
		}()
		go func() {
			defer wait.Done()
			<-start
			_, putErr = service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), racing)
		}()
		close(start)
		// Both writers must be observed blocked behind the holder before the
		// lock is released: real contention, not a timing guess.
		waitForBlockedSessions(t, h, holderPID, 2)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("release lock: %v", err)
		}
		wait.Wait()

		if finalizeErr != nil {
			t.Fatalf("finalize error = %v, want nil", finalizeErr)
		}
		runtime := h.runtime(t)
		// A committed PUT leaves the complete racing generation; a rejected PUT
		// leaves the complete previous generation. Nothing partial survives.
		want := racing
		if putErr != nil {
			if !errors.Is(putErr, finalread.ErrLocked) && !errors.Is(putErr, finalread.ErrConflict) {
				t.Fatalf("PUT error = %v, want ErrLocked/ErrConflict", putErr)
			}
			want = prior
		}
		assertMarksEqual(t, readMarks(t, runtime, finalID), want.Marks, "racing final marks")
		if got := readDecisions(t, runtime, finalID); !reflect.DeepEqual(got, want.CandidateDecisions) {
			t.Fatalf("racing final decisions = %+v, want %+v", got, want.CandidateDecisions)
		}
		if readLockedAt(t, runtime, finalID) == nil {
			t.Fatalf("final read was not locked")
		}
		if state, _ := caseStatus(t, runtime, caseID); state != "finalized" {
			t.Fatalf("case state = %q, want finalized", state)
		}
		if len(caseFinalizedEvents(t, runtime, caseID)) != 1 {
			t.Fatalf("case_finalized event count wrong")
		}
	})

	t.Run("GET never observes mixed marks and decisions", func(t *testing.T) {
		caseID, _, _ := seedLockedCase(t, pool, "1.2.3.4.5.5603")
		seedJob(t, pool, caseID, "succeeded", syntheticAISuccess(t, "1.2.3.4.5.5603", "1.2.3.4.7", "c1", "c2"), nil)

		generation := func(index int) finalread.Replacement {
			note := "gen" + strconv.Itoa(index)
			candidate := "c" + strconv.Itoa(index)
			return finalread.Replacement{
				Marks: []initialread.Mark{{
					P1MM:            []float64{1, 1, 1},
					P2MM:            []float64{2, 2, 2},
					CenterMM:        []float64{1.5, 1.5, 1.5},
					DiameterMM:      1,
					Origin:          finalread.OriginReader,
					ViewPlaneNormal: []float64{0, 0, 1},
					Note:            &note,
				}},
				CandidateDecisions: []finalread.Decision{{CandidateID: candidate, Decision: finalread.DecisionAccepted}},
			}
		}

		// A coherent generation exists before the writers start, so a reader
		// that never reads cannot pass vacuously.
		if _, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), generation(1)); err != nil {
			t.Fatalf("seed generation: %v", err)
		}

		var observed int32
		var wait sync.WaitGroup
		stop := make(chan struct{})
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer close(stop) // always released, even on writer error
			for index := 0; index < 40; index++ {
				if _, err := service.ReplaceFinal(ctx, caseID, readerUserID(t, pool), generation(1+index%2)); err != nil && !errors.Is(err, finalread.ErrLocked) {
					t.Errorf("ReplaceFinal: %v", err)
					return
				}
			}
		}()

		wait.Add(1)
		go func() {
			defer wait.Done()
			deadline := time.After(20 * time.Second)
			for {
				select {
				case <-stop:
					return
				case <-deadline:
					t.Errorf("reader did not terminate after the writer finished")
					return
				default:
				}
				read, err := store.GetFinal(ctx, caseID, readerUserID(t, pool))
				if err != nil {
					t.Errorf("GetFinal: %v", err)
					return
				}
				if len(read.Marks) == 0 {
					t.Errorf("reader observed an empty generation")
					return
				}
				if read.Marks[0].Note == nil {
					t.Errorf("mark note missing")
					return
				}
				note := *read.Marks[0].Note
				wantCandidate := "c" + note[len("gen"):]
				if len(read.CandidateDecisions) != 1 || read.CandidateDecisions[0].CandidateID != wantCandidate {
					t.Errorf("mixed snapshot: note %q decisions %+v", note, read.CandidateDecisions)
					return
				}
				atomic.AddInt32(&observed, 1)
			}
		}()
		wait.Wait()
		if atomic.LoadInt32(&observed) == 0 {
			t.Fatalf("reader observed no coherent snapshot")
		}
	})
}

// TestFinalizeLateAI proves a late job completion never changes the finalized
// physician snapshot, and that a terminal status reached before finalize is the
// one recorded.
func TestFinalizeLateAI(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := finalStore(t, h)
	job, _ := jobStore(t, h)

	series := func(index int) string { return "1.2.3.4.5.57" + strconv.Itoa(index) }

	insertMark := func(t *testing.T, readID int64, mark initialread.Mark) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, ai_candidate_id, view_plane_normal, note)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			readID, mark.P1MM, mark.P2MM, mark.CenterMM, mark.DiameterMM, mark.Origin, mark.AICandidateID, mark.ViewPlaneNormal, mark.Note); err != nil {
			t.Fatalf("insert mark: %v", err)
		}
	}

	// seedPopulatedLockedCase creates a case with a locked initial read that
	// already holds a mark, and an editable final read holding marks and both an
	// accepted and a dismissed decision.
	seedPopulatedLockedCase := func(t *testing.T, seriesUID string) (caseID, initialID, finalID int64) {
		t.Helper()
		caseID = seedCase(t, pool, seriesUID, "awaiting_read")
		initialID = seedInitialRead(t, pool, caseID)
		insertMark(t, initialID, markA())
		if _, err := pool.Exec(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1`, initialID); err != nil {
			t.Fatalf("lock initial read: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO reads (case_id, kind, reader_user_id) VALUES ($1, 'final', $2) RETURNING id`,
			caseID, readerUserID(t, pool)).Scan(&finalID); err != nil {
			t.Fatalf("seed final read: %v", err)
		}
		insertMark(t, finalID, markB())
		if _, err := pool.Exec(ctx,
			`INSERT INTO candidate_decisions (read_id, candidate_id, decision) VALUES ($1, 'c1', 'accepted')`, finalID); err != nil {
			t.Fatalf("seed accepted decision: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO candidate_decisions (read_id, candidate_id, decision, dismiss_reason) VALUES ($1, 'c2', 'dismissed', $2)`,
			finalID, "vessel, not a nodule"); err != nil {
			t.Fatalf("seed dismissed decision: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'initial_locked' WHERE id = $1`, caseID); err != nil {
			t.Fatalf("advance case: %v", err)
		}
		return caseID, initialID, finalID
	}

	// assertFrozen compares the full physician snapshot, both read locks, the
	// frozen finalize event and the decisions (by candidate id) after a late
	// terminal write.
	assertFrozen := func(t *testing.T, caseID, initialID, finalID int64, snapshot string, initialLocked, finalLocked *time.Time) {
		t.Helper()
		if after := snapshotClinical(t, pool); after != snapshot {
			t.Fatalf("physician snapshot changed after late completion:\nbefore %s\nafter  %s", snapshot, after)
		}
		runtime := h.runtime(t)
		if got := readLockedAt(t, runtime, initialID); got == nil || !got.Equal(*initialLocked) {
			t.Fatalf("initial lock changed: %v -> %v", initialLocked, got)
		}
		if got := readLockedAt(t, runtime, finalID); got == nil || !got.Equal(*finalLocked) {
			t.Fatalf("final lock changed: %v -> %v", finalLocked, got)
		}
		events := caseFinalizedEvents(t, runtime, caseID)
		if len(events) != 1 || events[0]["ai_status"] != "running" {
			t.Fatalf("finalize event changed: %+v", events)
		}
		decisions := readDecisions(t, runtime, finalID)
		if len(decisions) != 2 || decisions[0].CandidateID != "c1" || decisions[1].CandidateID != "c2" ||
			decisions[0].Decision != finalread.DecisionAccepted || decisions[1].Decision != finalread.DecisionDismissed {
			t.Fatalf("decisions changed: %+v", decisions)
		}
	}

	t.Run("late success after finalize does not alter the snapshot", func(t *testing.T) {
		caseID, initialID, finalID := seedPopulatedLockedCase(t, series(1))
		jobID := seedJob(t, pool, caseID, "queued", nil, nil)
		if _, ok, err := job.Claim(ctx, jobID); err != nil || !ok {
			t.Fatalf("Claim: %v %v", ok, err)
		}

		outcome, err := store.Finalize(ctx, caseID, readerUserID(t, pool))
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		if outcome.AIStatus != "running" {
			t.Fatalf("ai_status = %q, want running", outcome.AIStatus)
		}

		runtime := h.runtime(t)
		snapshot := snapshotClinical(t, pool)
		initialLocked := readLockedAt(t, runtime, initialID)
		finalLocked := readLockedAt(t, runtime, finalID)
		if initialLocked == nil || finalLocked == nil {
			t.Fatalf("finalize did not lock both reads")
		}

		if err := job.Succeed(ctx, jobID, syntheticAISuccess(t, series(1), "1.2.3.4.7", "c1", "c2")); err != nil {
			t.Fatalf("late Succeed: %v", err)
		}
		if status := jobStatus(t, pool, jobID); status != "succeeded" {
			t.Fatalf("job status = %q, want succeeded", status)
		}
		assertFrozen(t, caseID, initialID, finalID, snapshot, initialLocked, finalLocked)
	})

	t.Run("late failure after finalize does not alter the snapshot", func(t *testing.T) {
		caseID, initialID, finalID := seedPopulatedLockedCase(t, series(3))
		jobID := seedJob(t, pool, caseID, "queued", nil, nil)
		if _, ok, err := job.Claim(ctx, jobID); err != nil || !ok {
			t.Fatalf("Claim: %v %v", ok, err)
		}

		outcome, err := store.Finalize(ctx, caseID, readerUserID(t, pool))
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		if outcome.AIStatus != "running" {
			t.Fatalf("ai_status = %q, want running", outcome.AIStatus)
		}

		runtime := h.runtime(t)
		snapshot := snapshotClinical(t, pool)
		initialLocked := readLockedAt(t, runtime, initialID)
		finalLocked := readLockedAt(t, runtime, finalID)
		if initialLocked == nil || finalLocked == nil {
			t.Fatalf("finalize did not lock both reads")
		}

		if err := job.Fail(ctx, jobID, aijob.Failure{Code: aijob.CodeProcessError}); err != nil {
			t.Fatalf("late Fail: %v", err)
		}
		if status := jobStatus(t, pool, jobID); status != "failed" {
			t.Fatalf("job status = %q, want failed", status)
		}
		assertFrozen(t, caseID, initialID, finalID, snapshot, initialLocked, finalLocked)
	})

	t.Run("terminal state before finalize is recorded", func(t *testing.T) {
		caseID, _, _ := seedLockedCase(t, pool, series(2))
		jobID := seedJob(t, pool, caseID, "queued", nil, nil)
		if _, ok, err := job.Claim(ctx, jobID); err != nil || !ok {
			t.Fatalf("Claim: %v %v", ok, err)
		}
		if err := job.Succeed(ctx, jobID, syntheticAISuccess(t, series(2), "1.2.3.4.7", "c1")); err != nil {
			t.Fatalf("Succeed: %v", err)
		}
		outcome, err := store.Finalize(ctx, caseID, readerUserID(t, pool))
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		if outcome.AIStatus != "succeeded" {
			t.Fatalf("ai_status = %q, want succeeded", outcome.AIStatus)
		}
	})
}

// TestFinalizeCanceledUnderLock proves a canceled finalize records nothing and
// leaves the pool usable.
func TestFinalizeCanceledUnderLock(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := finalStore(t, h)

	caseID, _, finalID := seedLockedCase(t, pool, "1.2.3.4.5.5801")
	seedJob(t, pool, caseID, "queued", nil, nil)

	holder := h.runtime(t)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM cases WHERE id = $1 FOR UPDATE`, caseID); err != nil {
		t.Fatalf("hold case lock: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if _, err := store.Finalize(waitCtx, caseID, readerUserID(t, pool)); !errors.Is(err, finalread.ErrDependency) {
		t.Fatalf("error = %v, want ErrDependency", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release lock: %v", err)
	}

	runtime := h.runtime(t)
	state, finalizedAt := caseStatus(t, runtime, caseID)
	if state != "initial_locked" || finalizedAt != nil {
		t.Fatalf("canceled finalize changed the case: %q %v", state, finalizedAt)
	}
	if locked := readLockedAt(t, runtime, finalID); locked != nil {
		t.Fatalf("canceled finalize locked the final read")
	}
	if len(caseFinalizedEvents(t, runtime, caseID)) != 0 {
		t.Fatalf("canceled finalize recorded an event")
	}
	if _, err := store.GetFinal(ctx, caseID, readerUserID(t, pool)); err != nil {
		t.Fatalf("pool unusable after cancellation: %v", err)
	}
}
