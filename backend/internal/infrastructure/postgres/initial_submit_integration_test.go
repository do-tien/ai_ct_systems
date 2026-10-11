package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/initialread"
	"nodule-reader/backend/internal/infrastructure/postgres"
)

func submitStore(t *testing.T, h *storageHarness) (*postgres.InitialReadStore, *pgxpool.Pool) {
	t.Helper()
	pool := importRuntimePool(t, h)
	return postgres.NewInitialReadStore(pool), pool
}

func readIDByKind(t *testing.T, conn *pgx.Conn, caseID int64, kind string) (int64, bool) {
	t.Helper()
	var id int64
	err := conn.QueryRow(context.Background(), `SELECT id FROM reads WHERE case_id = $1 AND kind = $2`, caseID, kind).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read %s id: %v", kind, err)
	}
	return id, true
}

func readMarks(t *testing.T, conn *pgx.Conn, readID int64) []initialread.Mark {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT p1_mm, p2_mm, center_mm, diameter_mm, origin, ai_candidate_id, view_plane_normal, note
		 FROM marks WHERE read_id = $1 ORDER BY id`, readID)
	if err != nil {
		t.Fatalf("list marks: %v", err)
	}
	defer rows.Close()
	marks := []initialread.Mark{}
	for rows.Next() {
		var mark initialread.Mark
		var candidate, note *string
		if err := rows.Scan(&mark.P1MM, &mark.P2MM, &mark.CenterMM, &mark.DiameterMM, &mark.Origin, &candidate, &mark.ViewPlaneNormal, &note); err != nil {
			t.Fatalf("scan mark: %v", err)
		}
		mark.AICandidateID = candidate
		mark.Note = note
		marks = append(marks, mark)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("marks rows: %v", err)
	}
	return marks
}

func caseStatus(t *testing.T, conn *pgx.Conn, caseID int64) (string, *time.Time) {
	t.Helper()
	var status string
	var finalized *time.Time
	if err := conn.QueryRow(context.Background(), `SELECT status, finalized_at FROM cases WHERE id = $1`, caseID).Scan(&status, &finalized); err != nil {
		t.Fatalf("case status: %v", err)
	}
	return status, finalized
}

func countEvents(t *testing.T, conn *pgx.Conn, caseID int64, action string) int {
	t.Helper()
	var count int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE case_id = $1 AND action = $2`, caseID, action).Scan(&count); err != nil {
		t.Fatalf("event count: %v", err)
	}
	return count
}

func assertMarksEqual(t *testing.T, got, want []initialread.Mark, label string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s marks = %+v, want %+v", label, got, want)
	}
}

func TestSubmitInitialCopiesAndLocks(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := submitStore(t, h)
	runtime := h.runtime(t)

	caseID := seedCase(t, pool, "1.2.3.4.7.100", "awaiting_read")
	if _, err := pool.Exec(ctx, `INSERT INTO ai_jobs (case_id, status) VALUES ($1, 'running')`, caseID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	initial := []initialread.Mark{markA(), markB(), markC()}
	if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, initial); err != nil {
		t.Fatalf("seed marks: %v", err)
	}
	initialReadID, ok := readIDByKind(t, runtime, caseID, "initial")
	if !ok {
		t.Fatalf("initial read missing")
	}

	result, err := store.SubmitInitial(ctx, caseID, initialread.Reader)
	if err != nil {
		t.Fatalf("SubmitInitial: %v", err)
	}
	if result.Status != initialread.StatusInitialLocked || result.FinalReadID <= 0 {
		t.Fatalf("result = %+v", result)
	}

	// The returned timestamp is the database lock value on the initial row.
	locked := readLockedAt(t, runtime, initialReadID)
	if locked == nil {
		t.Fatalf("initial read not locked")
	}
	if !result.LockedAt.Equal(*locked) {
		t.Fatalf("returned locked_at = %v, database = %v", result.LockedAt, *locked)
	}

	status, finalized := caseStatus(t, runtime, caseID)
	if status != "initial_locked" || finalized != nil {
		t.Fatalf("case = %q finalized=%v", status, finalized)
	}

	finalReadID, ok := readIDByKind(t, runtime, caseID, "final")
	if !ok {
		t.Fatalf("final read missing")
	}
	if finalReadID != result.FinalReadID {
		t.Fatalf("final id = %d, result = %d", finalReadID, result.FinalReadID)
	}
	var finalReader string
	var finalLocked *time.Time
	var finalKind string
	if err := runtime.QueryRow(ctx, `SELECT kind, reader, locked_at FROM reads WHERE id = $1`, finalReadID).Scan(&finalKind, &finalReader, &finalLocked); err != nil {
		t.Fatalf("final read: %v", err)
	}
	if finalKind != "final" || finalReader != initialread.Reader || finalLocked != nil {
		t.Fatalf("final read = %q/%q/%v", finalKind, finalReader, finalLocked)
	}

	// Field-for-field ordered copy with new IDs, initial unchanged.
	assertMarksEqual(t, readMarks(t, runtime, initialReadID), initial, "initial")
	copied := readMarks(t, runtime, finalReadID)
	assertMarksEqual(t, copied, initial, "final")
	var reused int
	if err := runtime.QueryRow(ctx,
		`SELECT count(*) FROM marks a JOIN marks b ON a.id = b.id WHERE a.read_id = $1 AND b.read_id = $2`,
		initialReadID, finalReadID).Scan(&reused); err != nil {
		t.Fatalf("reused mark ids: %v", err)
	}
	if reused != 0 {
		t.Fatalf("copied marks reused ids")
	}

	// Audit event and untouched AI/decision state.
	if got := countEvents(t, runtime, caseID, "initial_submitted"); got != 1 {
		t.Fatalf("submit events = %d, want 1", got)
	}
	var detail []byte
	if err := runtime.QueryRow(ctx, `SELECT detail FROM events WHERE case_id = $1 AND action = 'initial_submitted'`, caseID).Scan(&detail); err != nil {
		t.Fatalf("event detail: %v", err)
	}
	var decoded struct {
		InitialReadID int64 `json:"initial_read_id"`
		FinalReadID   int64 `json:"final_read_id"`
	}
	if err := json.Unmarshal(detail, &decoded); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if decoded.InitialReadID != initialReadID || decoded.FinalReadID != finalReadID {
		t.Fatalf("detail = %+v", decoded)
	}
	var decisions int
	if err := runtime.QueryRow(ctx, `SELECT count(*) FROM candidate_decisions WHERE read_id IN ($1, $2)`, initialReadID, finalReadID).Scan(&decisions); err != nil {
		t.Fatalf("decisions: %v", err)
	}
	if decisions != 0 {
		t.Fatalf("submit created %d candidate decisions", decisions)
	}
	var jobStatus string
	if err := runtime.QueryRow(ctx, `SELECT status FROM ai_jobs WHERE case_id = $1`, caseID).Scan(&jobStatus); err != nil {
		t.Fatalf("job: %v", err)
	}
	if jobStatus != "running" {
		t.Fatalf("submit changed job status to %q", jobStatus)
	}
}

func TestSubmitInitialEmptyDraft(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := submitStore(t, h)
	runtime := h.runtime(t)

	caseID := seedCase(t, pool, "1.2.3.4.7.101", "awaiting_read")
	if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{}); err != nil {
		t.Fatalf("create empty draft: %v", err)
	}
	result, err := store.SubmitInitial(ctx, caseID, initialread.Reader)
	if err != nil {
		t.Fatalf("SubmitInitial: %v", err)
	}
	if result.FinalReadID <= 0 {
		t.Fatalf("result = %+v", result)
	}
	if got := readMarks(t, runtime, result.FinalReadID); len(got) != 0 {
		t.Fatalf("final marks = %+v, want empty", got)
	}
	if got := countEvents(t, runtime, caseID, "initial_submitted"); got != 1 {
		t.Fatalf("submit events = %d, want 1", got)
	}
}

func TestSubmitInitialMissingDraft(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := submitStore(t, h)
	runtime := h.runtime(t)

	caseID := seedCase(t, pool, "1.2.3.4.7.102", "awaiting_read")
	if _, err := store.SubmitInitial(ctx, caseID, initialread.Reader); !errors.Is(err, initialread.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if status, _ := caseStatus(t, runtime, caseID); status != "awaiting_read" {
		t.Fatalf("case status = %q", status)
	}
	if _, ok := readIDByKind(t, runtime, caseID, "final"); ok {
		t.Fatalf("missing draft created a final")
	}
	if got := countEvents(t, runtime, caseID, "initial_submitted"); got != 0 {
		t.Fatalf("events = %d, want 0", got)
	}
}

func TestSubmitInitialExistingFinal(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := submitStore(t, h)
	runtime := h.runtime(t)

	caseID := seedCase(t, pool, "1.2.3.4.7.103", "awaiting_read")
	if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markA()}); err != nil {
		t.Fatalf("seed marks: %v", err)
	}
	preexisting := seedReadKind(t, runtime, caseID, "final")

	if _, err := store.SubmitInitial(ctx, caseID, initialread.Reader); !errors.Is(err, initialread.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	// The preexisting final is not overwritten and the initial is not locked.
	if got := readMarks(t, runtime, preexisting); len(got) != 0 {
		t.Fatalf("preexisting final was modified: %+v", got)
	}
	initialID, _ := readIDByKind(t, runtime, caseID, "initial")
	if readLockedAt(t, runtime, initialID) != nil {
		t.Fatalf("initial was locked despite the conflict")
	}
	if status, _ := caseStatus(t, runtime, caseID); status != "awaiting_read" {
		t.Fatalf("case status = %q", status)
	}
	if got := countEvents(t, runtime, caseID, "initial_submitted"); got != 0 {
		t.Fatalf("events = %d, want 0", got)
	}
}

func TestSubmitInitialRepeated(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := submitStore(t, h)
	runtime := h.runtime(t)

	caseID := seedCase(t, pool, "1.2.3.4.7.104", "awaiting_read")
	initial := []initialread.Mark{markA(), markB()}
	if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, initial); err != nil {
		t.Fatalf("seed marks: %v", err)
	}
	first, err := store.SubmitInitial(ctx, caseID, initialread.Reader)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if _, err := store.SubmitInitial(ctx, caseID, initialread.Reader); !errors.Is(err, initialread.ErrLocked) {
		t.Fatalf("repeat error = %v, want ErrLocked", err)
	}
	var finals int
	if err := runtime.QueryRow(ctx, `SELECT count(*) FROM reads WHERE case_id = $1 AND kind = 'final'`, caseID).Scan(&finals); err != nil {
		t.Fatalf("final count: %v", err)
	}
	if finals != 1 {
		t.Fatalf("finals = %d, want 1", finals)
	}
	if got := countEvents(t, runtime, caseID, "initial_submitted"); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
	assertMarksEqual(t, readMarks(t, runtime, first.FinalReadID), initial, "final")
}

func TestSubmitInitialConcurrent(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := submitStore(t, h)
	runtime := h.runtime(t)

	caseID := seedCase(t, pool, "1.2.3.4.7.105", "awaiting_read")
	initial := []initialread.Mark{markA(), markB()}
	if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, initial); err != nil {
		t.Fatalf("seed marks: %v", err)
	}

	type outcome struct {
		result initialread.SubmitResult
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			result, err := store.SubmitInitial(ctx, caseID, initialread.Reader)
			results <- outcome{result: result, err: err}
		}()
	}
	ready.Wait()
	close(start)

	successes, locked := 0, 0
	var winner initialread.SubmitResult
	for range 2 {
		got := <-results
		switch {
		case got.err == nil:
			successes++
			winner = got.result
		case errors.Is(got.err, initialread.ErrLocked):
			locked++
		default:
			t.Fatalf("unexpected error: %v", got.err)
		}
	}
	if successes != 1 || locked != 1 {
		t.Fatalf("successes = %d locked = %d, want 1/1", successes, locked)
	}
	var finals int
	if err := runtime.QueryRow(ctx, `SELECT count(*) FROM reads WHERE case_id = $1 AND kind = 'final'`, caseID).Scan(&finals); err != nil {
		t.Fatalf("final count: %v", err)
	}
	if finals != 1 {
		t.Fatalf("finals = %d, want 1", finals)
	}
	if got := countEvents(t, runtime, caseID, "initial_submitted"); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
	assertMarksEqual(t, readMarks(t, runtime, winner.FinalReadID), initial, "final")
}

func TestSubmitInitialRacesReplace(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := submitStore(t, h)
	owner := h.owner(t)
	runtime := h.runtime(t)

	t.Run("replace first is copied whole", func(t *testing.T) {
		caseID := seedCase(t, pool, "1.2.3.4.7.106", "awaiting_read")
		initial := []initialread.Mark{markA(), markB()}
		if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, initial); err != nil {
			t.Fatalf("seed marks: %v", err)
		}
		initialID, _ := readIDByKind(t, runtime, caseID, "initial")

		// A direct writer holds the case lock and extends the list, racing the
		// submitter.
		writer, err := owner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin writer: %v", err)
		}
		if _, err := writer.Exec(ctx, `SELECT id FROM cases WHERE id = $1 FOR UPDATE`, caseID); err != nil {
			t.Fatalf("lock case: %v", err)
		}
		if _, err := writer.Exec(ctx,
			`INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, view_plane_normal, note)
			 VALUES ($1, '{5,5,5}'::float8[], '{6,6,6}'::float8[], '{5.5,5.5,5.5}'::float8[], 1.5, 'reader', '{0,0,1}'::float8[], 'raced')`,
			initialID); err != nil {
			t.Fatalf("writer insert: %v", err)
		}

		type result struct {
			out initialread.SubmitResult
			err error
		}
		done := make(chan result, 1)
		go func() {
			out, err := store.SubmitInitial(ctx, caseID, initialread.Reader)
			done <- result{out: out, err: err}
		}()
		select {
		case got := <-done:
			t.Fatalf("submit did not wait for the racing writer: %+v", got)
		case <-time.After(300 * time.Millisecond):
		}
		if err := writer.Commit(ctx); err != nil {
			t.Fatalf("writer commit: %v", err)
		}
		var got result
		select {
		case got = <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("submit never completed")
		}
		if got.err != nil {
			t.Fatalf("submit: %v", got.err)
		}
		// The final copies the committed list, including the raced mark.
		want := append(append([]initialread.Mark{}, initial...), initialread.Mark{
			P1MM: []float64{5, 5, 5}, P2MM: []float64{6, 6, 6}, CenterMM: []float64{5.5, 5.5, 5.5},
			DiameterMM: 1.5, Origin: initialread.Reader, ViewPlaneNormal: []float64{0, 0, 1}, Note: new("raced"),
		})
		assertMarksEqual(t, readMarks(t, runtime, got.out.FinalReadID), want, "raced final")
	})

	t.Run("submit first rejects the replace", func(t *testing.T) {
		caseID := seedCase(t, pool, "1.2.3.4.7.107", "awaiting_read")
		original := []initialread.Mark{markA()}
		if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, original); err != nil {
			t.Fatalf("seed marks: %v", err)
		}
		result, err := store.SubmitInitial(ctx, caseID, initialread.Reader)
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, []initialread.Mark{markB()}); !errors.Is(err, initialread.ErrLocked) {
			t.Fatalf("replace after submit = %v, want ErrLocked", err)
		}
		initialID, _ := readIDByKind(t, runtime, caseID, "initial")
		assertMarksEqual(t, readMarks(t, runtime, initialID), original, "locked initial")
		assertMarksEqual(t, readMarks(t, runtime, result.FinalReadID), original, "final")
	})
}

// installSubmitFailure installs an owner-controlled trigger that fails submit at
// one step, returning a cleanup that removes it. createTrigger contains a
// %FUNC% placeholder for the generated function name.
func installSubmitFailure(t *testing.T, h *storageHarness, name, createTrigger, body string) func() {
	t.Helper()
	owner := h.owner(t)
	ctx := context.Background()
	fn := "lc1_11_fail_" + name
	if _, err := owner.Exec(ctx, `CREATE OR REPLACE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $fn$ BEGIN `+body+` END $fn$`); err != nil {
		t.Fatalf("create fixture %s: %v", name, err)
	}
	statement := strings.ReplaceAll(createTrigger, "%FUNC%", fn)
	if _, err := owner.Exec(ctx, statement); err != nil {
		t.Fatalf("create trigger %s: %v", name, err)
	}
	if _, err := owner.Exec(ctx, `GRANT EXECUTE ON FUNCTION `+fn+`() TO `+h.runtimeRole); err != nil {
		t.Fatalf("grant fixture %s: %v", name, err)
	}
	return func() {
		if _, err := owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+fn+` ON `+tableOf(createTrigger)); err != nil {
			t.Fatalf("drop trigger %s: %v", name, err)
		}
		if _, err := owner.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`()`); err != nil {
			t.Fatalf("drop function %s: %v", name, err)
		}
	}
}

// tableOf extracts the table name from a CREATE TRIGGER statement.
func tableOf(createTrigger string) string {
	on := strings.Index(createTrigger, " ON ")
	rest := createTrigger[on+len(" ON "):]
	fields := strings.Fields(rest)
	return fields[0]
}

func TestSubmitInitialRollback(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := submitStore(t, h)
	owner := h.owner(t)
	runtime := h.runtime(t)

	fixtures := []struct {
		name          string
		createTrigger string
		body          string
		wantErr       error
	}{
		{
			name:          "final_insert",
			createTrigger: `CREATE TRIGGER %FUNC% BEFORE INSERT ON reads FOR EACH ROW WHEN (NEW.kind = 'final') EXECUTE FUNCTION %FUNC%()`,
			body:          `RAISE EXCEPTION 'lc1-11 final fixture';`,
			wantErr:       initialread.ErrDependency,
		},
		{
			name:          "copy_insert",
			createTrigger: `CREATE TRIGGER %FUNC% BEFORE INSERT ON marks FOR EACH ROW WHEN (NEW.note = 'lc1-11-copy-fixture') EXECUTE FUNCTION %FUNC%()`,
			body:          `RAISE EXCEPTION 'lc1-11 copy fixture';`,
			wantErr:       initialread.ErrDependency,
		},
		{
			name:          "case_update",
			createTrigger: `CREATE TRIGGER %FUNC% BEFORE UPDATE ON cases FOR EACH ROW WHEN (NEW.status = 'initial_locked') EXECUTE FUNCTION %FUNC%()`,
			body:          `RAISE EXCEPTION 'lc1-11 case fixture';`,
			wantErr:       initialread.ErrDependency,
		},
		{
			name:          "event_insert",
			createTrigger: `CREATE TRIGGER %FUNC% BEFORE INSERT ON events FOR EACH ROW WHEN (NEW.action = 'initial_submitted') EXECUTE FUNCTION %FUNC%()`,
			body:          `RAISE EXCEPTION 'lc1-11 event fixture';`,
			wantErr:       initialread.ErrDependency,
		},
		{
			name:          "deferred_commit",
			createTrigger: `CREATE CONSTRAINT TRIGGER %FUNC% AFTER INSERT ON marks DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.note = 'lc1-11-deferred-fixture') EXECUTE FUNCTION %FUNC%()`,
			body:          `RAISE EXCEPTION 'lc1-11 deferred fixture';`,
			wantErr:       initialread.ErrDependency,
		},
	}

	for index, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			caseID := seedCase(t, pool, "1.2.3.4.7."+strconv.Itoa(200+index), "awaiting_read")
			var initial []initialread.Mark
			switch fixture.name {
			case "copy_insert":
				initial = []initialread.Mark{markA(), {P1MM: []float64{1, 1, 1}, P2MM: []float64{2, 2, 2}, CenterMM: []float64{1.5, 1.5, 1.5}, DiameterMM: 1, Origin: initialread.Reader, ViewPlaneNormal: []float64{0, 0, 1}, Note: new("lc1-11-copy-fixture")}}
			case "deferred_commit":
				initial = []initialread.Mark{markA(), {P1MM: []float64{1, 1, 1}, P2MM: []float64{2, 2, 2}, CenterMM: []float64{1.5, 1.5, 1.5}, DiameterMM: 1, Origin: initialread.Reader, ViewPlaneNormal: []float64{0, 0, 1}, Note: new("lc1-11-deferred-fixture")}}
			default:
				initial = []initialread.Mark{markA(), markB()}
			}
			if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, initial); err != nil {
				t.Fatalf("seed marks: %v", err)
			}
			initialID, _ := readIDByKind(t, runtime, caseID, "initial")

			cleanup := installSubmitFailure(t, h, fixture.name, fixture.createTrigger, fixture.body)
			defer cleanup()

			_, err := store.SubmitInitial(ctx, caseID, initialread.Reader)
			if !errors.Is(err, fixture.wantErr) {
				t.Fatalf("error = %v, want %v", err, fixture.wantErr)
			}

			if readLockedAt(t, runtime, initialID) != nil {
				t.Fatalf("initial was locked despite the failure")
			}
			assertMarksEqual(t, readMarks(t, runtime, initialID), initial, "initial after failure")
			if status, _ := caseStatus(t, runtime, caseID); status != "awaiting_read" {
				t.Fatalf("case status = %q", status)
			}
			if _, ok := readIDByKind(t, runtime, caseID, "final"); ok {
				t.Fatalf("failed submit left a final read")
			}
			if got := countEvents(t, runtime, caseID, "initial_submitted"); got != 0 {
				t.Fatalf("failed submit left %d events", got)
			}

			// The connection is reusable and a clean submit then succeeds.
			cleanup()
			if _, err := owner.Exec(ctx, `SELECT 1`); err != nil {
				t.Fatalf("owner connection unusable: %v", err)
			}
			result, err := store.SubmitInitial(ctx, caseID, initialread.Reader)
			if err != nil {
				t.Fatalf("recovered submit: %v", err)
			}
			assertMarksEqual(t, readMarks(t, runtime, result.FinalReadID), initial, "recovered final")
		})
	}
}

func TestSubmitInitialCanceledWhileWaiting(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := submitStore(t, h)
	owner := h.owner(t)
	runtime := h.runtime(t)

	caseID := seedCase(t, pool, "1.2.3.4.7.300", "awaiting_read")
	initial := []initialread.Mark{markA()}
	if _, err := store.ReplaceInitialMarks(ctx, caseID, initialread.Reader, initial); err != nil {
		t.Fatalf("seed marks: %v", err)
	}
	initialID, _ := readIDByKind(t, runtime, caseID, "initial")

	blocker, err := owner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `SELECT id FROM cases WHERE id = $1 FOR UPDATE`, caseID); err != nil {
		t.Fatalf("lock case: %v", err)
	}

	canceled, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := store.SubmitInitial(canceled, caseID, initialread.Reader); !errors.Is(err, initialread.ErrDependency) {
		t.Fatalf("canceled submit error = %v, want ErrDependency", err)
	}
	if readLockedAt(t, runtime, initialID) != nil {
		t.Fatalf("canceled submit locked the initial")
	}
	if _, ok := readIDByKind(t, runtime, caseID, "final"); ok {
		t.Fatalf("canceled submit left a final")
	}
	if got := countEvents(t, runtime, caseID, "initial_submitted"); got != 0 {
		t.Fatalf("canceled submit left %d events", got)
	}

	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("blocker commit: %v", err)
	}
	result, err := store.SubmitInitial(ctx, caseID, initialread.Reader)
	if err != nil {
		t.Fatalf("submit after release: %v (connection not released?)", err)
	}
	assertMarksEqual(t, readMarks(t, runtime, result.FinalReadID), initial, "final")
}
