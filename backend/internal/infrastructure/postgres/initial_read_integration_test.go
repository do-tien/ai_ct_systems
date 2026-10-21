package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/initialread"
	"nodule-reader/backend/internal/infrastructure/postgres"
	httpapi "nodule-reader/backend/internal/transport/http"
)

// initialStore builds the runtime-role adapter on a fresh disposable database.
func initialStore(t *testing.T, h *storageHarness) (*postgres.InitialReadStore, *pgxpool.Pool) {
	t.Helper()
	pool := importRuntimePool(t, h)
	return postgres.NewInitialReadStore(pool), pool
}

func seedCase(t *testing.T, pool *pgxpool.Pool, seriesUID, status string) int64 {
	t.Helper()
	ctx := context.Background()
	var caseID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO cases (source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid, status)
		 VALUES ('LC1-10/SYNTHETIC', '1.2.3.4.5.99', $1, 'orthanc-1', '1.2.3.4.7', $2) RETURNING id`,
		seriesUID, status).Scan(&caseID); err != nil {
		t.Fatalf("seed case: %v", err)
	}
	return caseID
}

func seedInitialRead(t *testing.T, pool *pgxpool.Pool, caseID int64) int64 {
	t.Helper()
	ctx := context.Background()
	var readID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO reads (case_id, kind, reader) VALUES ($1, 'initial', $2) RETURNING id`,
		caseID, initialread.Reader).Scan(&readID); err != nil {
		t.Fatalf("seed read: %v", err)
	}
	return readID
}

func sampleMark(p1, p2, center []float64, diameter float64, note *string) initialread.Mark {
	return initialread.Mark{
		P1MM:            p1,
		P2MM:            p2,
		CenterMM:        center,
		DiameterMM:      diameter,
		Origin:          initialread.OriginReader,
		ViewPlaneNormal: []float64{0, 0, 1},
		Note:            note,
	}
}

func markA() initialread.Mark {
	return sampleMark(
		[]float64{-102.3, 45.1, -180.5},
		[]float64{-90.2, 33.0, -180.5},
		[]float64{-96.25, 39.05, -180.5},
		17.0, nil)
}

func markB() initialread.Mark {
	return sampleMark(
		[]float64{12.5, -3.25, 44.0},
		[]float64{20.5, -9.25, 44.0},
		[]float64{16.5, -6.25, 44.0},
		8.5, new("spiculated margin"))
}

func markC() initialread.Mark {
	return sampleMark(
		[]float64{0.5, 0.75, -1.25},
		[]float64{4.5, 3.75, -1.25},
		[]float64{2.5, 2.25, -1.25},
		4.0, new("stable"))
}

func TestInitialReadStore(t *testing.T) {
	ctx := context.Background()

	t.Run("metadata and not found", func(t *testing.T) {
		h := newStorageHarness(t)
		h.migrate(t)
		h.grantRuntime(t)
		store, pool := initialStore(t, h)

		caseID := seedCase(t, pool, "1.2.3.4.5.1001", "awaiting_read")
		got, err := store.GetCase(ctx, caseID)
		if err != nil {
			t.Fatalf("GetCase: %v", err)
		}
		if got.ID != caseID || got.Source != "LC1-10/SYNTHETIC" || got.SeriesInstanceUID != "1.2.3.4.5.1001" ||
			got.FrameOfReferenceUID != "1.2.3.4.7" || got.Status != "awaiting_read" {
			t.Fatalf("case metadata = %+v", got)
		}
		if got.CreatedAt.IsZero() || got.FinalizedAt != nil {
			t.Fatalf("timestamps = created %v finalized %v", got.CreatedAt, got.FinalizedAt)
		}
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM reads WHERE case_id = $1`, caseID); got != 0 {
			t.Fatalf("reads = %d, want 0 (metadata must not create a draft)", got)
		}

		if _, err := store.GetCase(ctx, 99999999); !errors.Is(err, initialread.ErrNotFound) {
			t.Fatalf("missing GetCase error = %v, want ErrNotFound", err)
		}
		if _, err := store.GetOrCreateInitial(ctx, 99999999, initialread.Reader); !errors.Is(err, initialread.ErrNotFound) {
			t.Fatalf("missing GetInitial error = %v, want ErrNotFound", err)
		}
		if _, err := store.ReplaceInitialMarks(ctx, 99999999, initialread.Reader, []initialread.Mark{markA()}); !errors.Is(err, initialread.ErrNotFound) {
			t.Fatalf("missing ReplaceInitialMarks error = %v, want ErrNotFound", err)
		}
	})

	t.Run("concurrent first GET returns one draft", func(t *testing.T) {
		h := newStorageHarness(t)
		h.migrate(t)
		h.grantRuntime(t)
		store, pool := initialStore(t, h)
		caseID := seedCase(t, pool, "1.2.3.4.5.1002", "awaiting_read")

		type outcome struct {
			read initialread.Read
			err  error
		}
		results := make(chan outcome, 2)
		// A start barrier forces both calls to be in flight together instead of
		// letting the scheduler run them mostly sequentially.
		start := make(chan struct{})
		var ready sync.WaitGroup
		ready.Add(2)
		for range 2 {
			go func() {
				ready.Done()
				<-start
				read, err := store.GetOrCreateInitial(ctx, caseID, initialread.Reader)
				results <- outcome{read: read, err: err}
			}()
		}
		ready.Wait()
		close(start)
		first, second := <-results, <-results
		if first.err != nil || second.err != nil {
			t.Fatalf("errors = %v / %v", first.err, second.err)
		}
		if first.read.ID != second.read.ID {
			t.Fatalf("draft ids = %d / %d, want equal", first.read.ID, second.read.ID)
		}
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM reads WHERE case_id = $1`, caseID); got != 1 {
			t.Fatalf("reads = %d, want 1", got)
		}
	})

	t.Run("PUT before GET creates the draft", func(t *testing.T) {
		h := newStorageHarness(t)
		h.migrate(t)
		h.grantRuntime(t)
		store, pool := initialStore(t, h)
		caseID := seedCase(t, pool, "1.2.3.4.5.1003", "awaiting_read")

		saved, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markA(), markB()})
		if err != nil {
			t.Fatalf("ReplaceInitialMarks: %v", err)
		}
		if len(saved) != 2 {
			t.Fatalf("saved = %d marks, want 2", len(saved))
		}
		read, err := store.GetOrCreateInitial(ctx, caseID, initialread.Reader)
		if err != nil {
			t.Fatalf("GetInitial: %v", err)
		}
		if read.Reader != initialread.Reader || read.Kind != initialread.KindInitial || read.LockedAt != nil {
			t.Fatalf("read = %+v", read)
		}
		if !reflect.DeepEqual(read.Marks, []initialread.Mark{markA(), markB()}) {
			t.Fatalf("marks = %+v", read.Marks)
		}
	})

	t.Run("complete replacement and clear", func(t *testing.T) {
		h := newStorageHarness(t)
		h.migrate(t)
		h.grantRuntime(t)
		store, pool := initialStore(t, h)
		caseID := seedCase(t, pool, "1.2.3.4.5.1004", "awaiting_read")

		if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markA(), markB()}); err != nil {
			t.Fatalf("first replace: %v", err)
		}
		replaced, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markC()})
		if err != nil {
			t.Fatalf("second replace: %v", err)
		}
		if !reflect.DeepEqual(replaced, []initialread.Mark{markC()}) {
			t.Fatalf("second replace returned %+v", replaced)
		}
		read, err := store.GetOrCreateInitial(ctx, caseID, initialread.Reader)
		if err != nil {
			t.Fatalf("GetInitial after replace: %v", err)
		}
		if !reflect.DeepEqual(read.Marks, []initialread.Mark{markC()}) {
			t.Fatalf("marks after replace = %+v, want only markC", read.Marks)
		}

		cleared, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{})
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if cleared == nil || len(cleared) != 0 {
			t.Fatalf("cleared = %#v, want a non-nil empty slice", cleared)
		}
		read, err = store.GetOrCreateInitial(ctx, caseID, initialread.Reader)
		if err != nil {
			t.Fatalf("GetInitial after clear: %v", err)
		}
		if read.Marks == nil || len(read.Marks) != 0 {
			t.Fatalf("marks after clear = %#v, want non-nil empty", read.Marks)
		}
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM marks`); got != 0 {
			t.Fatalf("mark rows = %d, want 0", got)
		}
	})

	t.Run("order and signed coordinates round trip", func(t *testing.T) {
		h := newStorageHarness(t)
		h.migrate(t)
		h.grantRuntime(t)
		store, pool := initialStore(t, h)
		caseID := seedCase(t, pool, "1.2.3.4.5.1005", "awaiting_read")

		normalMark := markA()
		normalMark.ViewPlaneNormal = []float64{0, 0, 2.5}
		input := []initialread.Mark{markB(), normalMark, markC()}
		if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, input); err != nil {
			t.Fatalf("replace: %v", err)
		}
		read, err := store.GetOrCreateInitial(ctx, caseID, initialread.Reader)
		if err != nil {
			t.Fatalf("GetInitial: %v", err)
		}
		if !reflect.DeepEqual(read.Marks, input) {
			t.Fatalf("marks = %+v, want %+v", read.Marks, input)
		}
	})

	t.Run("locked and later-state writes rejected", func(t *testing.T) {
		h := newStorageHarness(t)
		h.migrate(t)
		h.grantRuntime(t)
		store, pool := initialStore(t, h)
		caseID := seedCase(t, pool, "1.2.3.4.5.1006", "awaiting_read")
		readID := seedInitialRead(t, pool, caseID)

		if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markA(), markB()}); err != nil {
			t.Fatalf("seed marks: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1`, readID); err != nil {
			t.Fatalf("lock read: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'initial_locked' WHERE id = $1`, caseID); err != nil {
			t.Fatalf("lock case: %v", err)
		}

		if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markC()}); !errors.Is(err, initialread.ErrLocked) {
			t.Fatalf("locked replace error = %v, want ErrLocked", err)
		}
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM marks WHERE read_id = $1`, readID); got != 2 {
			t.Fatalf("marks after rejected write = %d, want 2", got)
		}

		read, err := store.GetOrCreateInitial(ctx, caseID, initialread.Reader)
		if err != nil {
			t.Fatalf("locked GetInitial: %v", err)
		}
		if read.LockedAt == nil {
			t.Fatalf("locked read shows no locked_at")
		}
		if !reflect.DeepEqual(read.Marks, []initialread.Mark{markA(), markB()}) {
			t.Fatalf("locked marks = %+v", read.Marks)
		}

		if _, err := pool.Exec(ctx, `UPDATE cases SET status = 'finalized', finalized_at = now() WHERE id = $1`, caseID); err != nil {
			t.Fatalf("finalize case: %v", err)
		}
		if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{}); !errors.Is(err, initialread.ErrLocked) {
			t.Fatalf("finalized replace error = %v, want ErrLocked", err)
		}
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM marks WHERE read_id = $1`, readID); got != 2 {
			t.Fatalf("marks after finalized rejection = %d, want 2", got)
		}
	})

	t.Run("later-state case without a read conflicts", func(t *testing.T) {
		h := newStorageHarness(t)
		h.migrate(t)
		h.grantRuntime(t)
		store, pool := initialStore(t, h)

		locked := seedCase(t, pool, "1.2.3.4.5.1007", "initial_locked")
		if _, err := store.GetOrCreateInitial(ctx, locked, initialread.Reader); !errors.Is(err, initialread.ErrLocked) {
			t.Fatalf("GetInitial later-state error = %v, want ErrLocked", err)
		}
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM reads WHERE case_id = $1`, locked); got != 0 {
			t.Fatalf("reads = %d, want 0 (must not fabricate a draft)", got)
		}
		if _, err := store.ReplaceInitialMarks(ctx, locked, initialread.Reader, []initialread.Mark{markA()}); !errors.Is(err, initialread.ErrLocked) {
			t.Fatalf("ReplaceInitialMarks later-state error = %v, want ErrLocked", err)
		}

		finalized := seedCase(t, pool, "1.2.3.4.5.1008", "finalized")
		if _, err := store.GetOrCreateInitial(ctx, finalized, initialread.Reader); !errors.Is(err, initialread.ErrLocked) {
			t.Fatalf("finalized GetInitial error = %v, want ErrLocked", err)
		}
	})
}

// installMarkRejectTrigger installs an owner-controlled trigger that rejects an
// insert whose note equals the designated fixture text. It is removed on
// cleanup; the production schema is never modified.
func installMarkRejectTrigger(t *testing.T, h *storageHarness) {
	t.Helper()
	owner := h.owner(t)
	ctx := context.Background()
	if _, err := owner.Exec(ctx,
		`CREATE OR REPLACE FUNCTION lc1_10_fail_marks() RETURNS trigger LANGUAGE plpgsql AS $fn$
		 BEGIN
		   IF NEW.note = 'lc1-10-rollback-fixture' THEN
		     RAISE EXCEPTION 'lc1-10 rollback fixture';
		   END IF;
		   RETURN NEW;
		 END
		 $fn$`); err != nil {
		t.Fatalf("create fixture function: %v", err)
	}
	if _, err := owner.Exec(ctx,
		`CREATE TRIGGER lc1_10_fail_marks BEFORE INSERT ON marks FOR EACH ROW EXECUTE FUNCTION lc1_10_fail_marks()`); err != nil {
		t.Fatalf("create fixture trigger: %v", err)
	}
	if _, err := owner.Exec(ctx, `GRANT EXECUTE ON FUNCTION lc1_10_fail_marks() TO `+h.runtimeRole); err != nil {
		t.Fatalf("grant fixture: %v", err)
	}
	t.Cleanup(func() {
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS lc1_10_fail_marks ON marks`,
			`DROP FUNCTION IF EXISTS lc1_10_fail_marks()`,
		} {
			if _, err := owner.Exec(context.Background(), statement); err != nil {
				t.Fatalf("cleanup %q: %v", statement, err)
			}
		}
	})
}

func TestInitialReadReplacementRollback(t *testing.T) {
	ctx := context.Background()
	fixtureNote := func() *string { return new("lc1-10-rollback-fixture") }

	failingSecondMark := markC()
	failingSecondMark.Note = fixtureNote()

	t.Run("original list survives a failed replacement", func(t *testing.T) {
		h := newStorageHarness(t)
		h.migrate(t)
		h.grantRuntime(t)
		store, pool := initialStore(t, h)
		caseID := seedCase(t, pool, "1.2.3.4.5.1101", "awaiting_read")
		seedInitialRead(t, pool, caseID)
		if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markA(), markB()}); err != nil {
			t.Fatalf("seed marks: %v", err)
		}

		installMarkRejectTrigger(t, h)

		_, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markA(), failingSecondMark})
		if !errors.Is(err, initialread.ErrDependency) {
			t.Fatalf("replacement error = %v, want ErrDependency", err)
		}
		read, err := store.GetOrCreateInitial(ctx, caseID, initialread.Reader)
		if err != nil {
			t.Fatalf("GetInitial: %v", err)
		}
		if !reflect.DeepEqual(read.Marks, []initialread.Mark{markA(), markB()}) {
			t.Fatalf("marks = %+v, want the original two rows intact", read.Marks)
		}
	})

	t.Run("draft creation rolls back when the first insert fails", func(t *testing.T) {
		h := newStorageHarness(t)
		h.migrate(t)
		h.grantRuntime(t)
		store, pool := initialStore(t, h)
		caseID := seedCase(t, pool, "1.2.3.4.5.1102", "awaiting_read")

		installMarkRejectTrigger(t, h)

		failing := markA()
		failing.Note = fixtureNote()
		if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{failing}); !errors.Is(err, initialread.ErrDependency) {
			t.Fatalf("replacement error = %v, want ErrDependency", err)
		}
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM reads WHERE case_id = $1`, caseID); got != 0 {
			t.Fatalf("reads = %d, want 0 (draft must roll back)", got)
		}
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM marks`); got != 0 {
			t.Fatalf("marks = %d, want 0", got)
		}
	})
}

func TestInitialReadSerializedSnapshot(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := initialStore(t, h)
	caseID := seedCase(t, pool, "1.2.3.4.5.1201", "awaiting_read")
	readID := seedInitialRead(t, pool, caseID)
	if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markA(), markB()}); err != nil {
		t.Fatalf("seed marks: %v", err)
	}

	owner := h.owner(t)
	ownerTx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin owner tx: %v", err)
	}
	if _, err := ownerTx.Exec(ctx, `SELECT id FROM cases WHERE id = $1 FOR UPDATE`, caseID); err != nil {
		t.Fatalf("lock case: %v", err)
	}
	if _, err := ownerTx.Exec(ctx, `DELETE FROM marks WHERE read_id = $1`, readID); err != nil {
		t.Fatalf("delete marks: %v", err)
	}
	for _, mark := range []initialread.Mark{markA(), markB(), markC()} {
		if _, err := ownerTx.Exec(ctx,
			`INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, ai_candidate_id, view_plane_normal, note)
			 VALUES ($1, $2, $3, $4, $5, 'reader', NULL, $6, $7)`,
			readID, mark.P1MM, mark.P2MM, mark.CenterMM, mark.DiameterMM, mark.ViewPlaneNormal, mark.Note); err != nil {
			t.Fatalf("insert replacement mark: %v", err)
		}
	}

	type outcome struct {
		read initialread.Read
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		read, err := store.GetOrCreateInitial(ctx, caseID, initialread.Reader)
		done <- outcome{read: read, err: err}
	}()

	select {
	case got := <-done:
		t.Fatalf("GetInitial observed uncommitted marks: %+v (err %v)", got.read.Marks, got.err)
	case <-time.After(400 * time.Millisecond):
	}

	if err := ownerTx.Commit(ctx); err != nil {
		t.Fatalf("commit owner tx: %v", err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("GetInitial after commit: %v", got.err)
		}
		if !reflect.DeepEqual(got.read.Marks, []initialread.Mark{markA(), markB(), markC()}) {
			t.Fatalf("marks = %+v, want the committed full list", got.read.Marks)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("GetInitial did not return after the lock was released")
	}
}

func TestInitialReadCanceledUnderLock(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := initialStore(t, h)
	caseID := seedCase(t, pool, "1.2.3.4.5.1301", "awaiting_read")
	readID := seedInitialRead(t, pool, caseID)
	if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markA()}); err != nil {
		t.Fatalf("seed marks: %v", err)
	}

	owner := h.owner(t)
	ownerTx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin owner tx: %v", err)
	}
	defer ownerTx.Rollback(ctx)
	if _, err := ownerTx.Exec(ctx, `SELECT id FROM cases WHERE id = $1 FOR UPDATE`, caseID); err != nil {
		t.Fatalf("lock case: %v", err)
	}

	canceled, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := store.GetOrCreateInitial(canceled, caseID, initialread.Reader); !errors.Is(err, initialread.ErrDependency) {
		t.Fatalf("canceled GetInitial error = %v, want ErrDependency", err)
	}
	if got := countRows(t, ctx, pool, `SELECT count(*) FROM marks WHERE read_id = $1`, readID); got != 1 {
		t.Fatalf("marks after canceled read = %d, want 1 (no writes)", got)
	}

	if err := ownerTx.Commit(ctx); err != nil {
		t.Fatalf("commit owner tx: %v", err)
	}
	read, err := store.GetOrCreateInitial(ctx, caseID, initialread.Reader)
	if err != nil {
		t.Fatalf("GetInitial after release: %v (connection not released?)", err)
	}
	if !reflect.DeepEqual(read.Marks, []initialread.Mark{markA()}) {
		t.Fatalf("marks = %+v", read.Marks)
	}
}

// wireMarkJSON encodes a mark in the exact PUT wire shape.
func wireMarkJSON(t *testing.T, mark initialread.Mark) string {
	t.Helper()
	payload := struct {
		P1        []float64 `json:"p1_mm"`
		P2        []float64 `json:"p2_mm"`
		Center    []float64 `json:"center_mm"`
		Diameter  float64   `json:"diameter_mm"`
		Origin    string    `json:"origin"`
		Candidate *string   `json:"ai_candidate_id"`
		Normal    []float64 `json:"view_plane_normal"`
		Note      *string   `json:"note"`
	}{mark.P1MM, mark.P2MM, mark.CenterMM, mark.DiameterMM, mark.Origin, mark.AICandidateID, mark.ViewPlaneNormal, mark.Note}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode mark: %v", err)
	}
	return string(encoded)
}

// TestInitialReadHTTPIntegration drives the real router, service and adapter
// against a real PostgreSQL runtime login, and proves the case response is
// independent of AI-job state.
func TestInitialReadHTTPIntegration(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	pool := importRuntimePool(t, h)
	store := postgres.NewInitialReadStore(pool)
	handler := httpapi.NewRouter(nil, initialread.NewService(store), nil, nil, http.NotFoundHandler())

	caseID := seedCase(t, pool, "1.2.3.4.5.1401", "awaiting_read")
	if _, err := pool.Exec(ctx, `INSERT INTO ai_jobs (case_id, status) VALUES ($1, 'queued')`, caseID); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	do := func(method, target, contentType, body string) *httptest.ResponseRecorder {
		var reader *strings.Reader
		if body != "" {
			reader = strings.NewReader(body)
		} else {
			reader = strings.NewReader("")
		}
		request := httptest.NewRequest(method, target, reader)
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	replace := `{"marks":[` + wireMarkJSON(t, markA()) + `,` + wireMarkJSON(t, markB()) + `]}`
	putPath := fmt.Sprintf("/api/cases/%d/reads/initial/marks", caseID)
	recorder := do(http.MethodPut, putPath, "application/json", replace)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	readRecorder := do(http.MethodGet, fmt.Sprintf("/api/cases/%d/reads/initial", caseID), "", "")
	if readRecorder.Code != http.StatusOK {
		t.Fatalf("GET initial status = %d (%s)", readRecorder.Code, readRecorder.Body.String())
	}
	var readBody struct {
		ID     int64  `json:"id"`
		CaseID int64  `json:"case_id"`
		Kind   string `json:"kind"`
		Reader string `json:"reader"`
		Marks  []struct {
			P1       []float64 `json:"p1_mm"`
			P2       []float64 `json:"p2_mm"`
			Center   []float64 `json:"center_mm"`
			Diameter float64   `json:"diameter_mm"`
			Origin   string    `json:"origin"`
			Normal   []float64 `json:"view_plane_normal"`
			Note     *string   `json:"note"`
		} `json:"marks"`
	}
	if err := json.Unmarshal(readRecorder.Body.Bytes(), &readBody); err != nil {
		t.Fatalf("decode read: %v", err)
	}
	if readBody.CaseID != caseID || readBody.Kind != "initial" || readBody.Reader != "reader" {
		t.Fatalf("read identity = %+v", readBody)
	}
	if len(readBody.Marks) != 2 {
		t.Fatalf("marks = %d, want 2", len(readBody.Marks))
	}
	if !reflect.DeepEqual(readBody.Marks[0].P1, markA().P1MM) ||
		!reflect.DeepEqual(readBody.Marks[0].P2, markA().P2MM) ||
		!reflect.DeepEqual(readBody.Marks[0].Center, markA().CenterMM) ||
		readBody.Marks[0].Diameter != markA().DiameterMM ||
		readBody.Marks[0].Note != nil ||
		readBody.Marks[1].Note == nil || *readBody.Marks[1].Note != "spiculated margin" {
		t.Fatalf("persisted geometry = %+v", readBody.Marks)
	}

	casePath := fmt.Sprintf("/api/cases/%d", caseID)
	before := do(http.MethodGet, casePath, "", "")
	if before.Code != http.StatusOK {
		t.Fatalf("GET case status = %d", before.Code)
	}
	var caseObject map[string]json.RawMessage
	if err := json.Unmarshal(before.Body.Bytes(), &caseObject); err != nil {
		t.Fatalf("decode case: %v", err)
	}
	wantKeys := []string{"created_at", "finalized_at", "frame_of_reference_uid", "id", "series_instance_uid", "source", "status", "study_instance_uid"}
	gotKeys := make([]string, 0, len(caseObject))
	for key := range caseObject {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)
	if strings.Join(gotKeys, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("case keys = %v, want %v", gotKeys, wantKeys)
	}

	owner := h.owner(t)
	if _, err := owner.Exec(ctx,
		`UPDATE ai_jobs SET status = 'succeeded', result = '{"candidates":[{"id":"c1","score":0.9}]}'::jsonb, finished_at = now() WHERE case_id = $1`,
		caseID); err != nil {
		t.Fatalf("update job: %v", err)
	}
	if _, err := owner.Exec(ctx, `UPDATE ai_jobs SET status = 'failed', error_code = 'MODEL_ERROR', result = NULL WHERE case_id = $1`, caseID); err != nil {
		t.Fatalf("fail job: %v", err)
	}
	after := do(http.MethodGet, casePath, "", "")
	if after.Body.String() != before.Body.String() {
		t.Fatalf("case response changed with AI job state:\n before %s\n after  %s", before.Body.String(), after.Body.String())
	}
	for _, forbidden := range []string{"candidate", "score", "model", "succeeded", "failed"} {
		if strings.Contains(after.Body.String(), forbidden) {
			t.Fatalf("case response leaked %q: %s", forbidden, after.Body.String())
		}
	}
}
