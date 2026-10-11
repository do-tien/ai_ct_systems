package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/caseimport"
	"nodule-reader/backend/internal/infrastructure/postgres"
)

// importInput is one synthetic validated import. Series UIDs are unique per
// test so no real identifier ever reaches the database.
func importInput(seriesUID, studyUID, source string) caseimport.ImportedCase {
	return caseimport.ImportedCase{
		Source:              source,
		StudyInstanceUID:    studyUID,
		SeriesInstanceUID:   seriesUID,
		OrthancSeriesID:     "11111111-22222222-33333333-44444444-55555555",
		FrameOfReferenceUID: "1.2.840.113619.2.55.3.604688.100.3",
		InstanceCount:       158,
	}
}

func importRuntimePool(t *testing.T, h *storageHarness) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), h.runtimeDSN)
	if err != nil {
		t.Fatalf("runtime pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("runtime ping: %v", err)
	}
	return pool
}

func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, query, args...).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return count
}

func TestImportStoreAtomic(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	pool := importRuntimePool(t, h)
	store := postgres.NewImportStore(pool)
	ctx := context.Background()

	input := importInput("1.2.3.4.5.100", "1.2.3.4.5.99", "LIDC-IDRI")
	caseID, err := store.CreateImportedCase(ctx, input)
	if err != nil {
		t.Fatalf("CreateImportedCase: %v", err)
	}

	var (
		source, studyUID, seriesUID, orthancID, frameUID, status string
	)
	if err := pool.QueryRow(ctx,
		`SELECT source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid, status
		 FROM cases WHERE id = $1`, caseID).
		Scan(&source, &studyUID, &seriesUID, &orthancID, &frameUID, &status); err != nil {
		t.Fatalf("read case: %v", err)
	}
	if source != input.Source || studyUID != input.StudyInstanceUID || seriesUID != input.SeriesInstanceUID ||
		orthancID != input.OrthancSeriesID || frameUID != input.FrameOfReferenceUID || status != "awaiting_read" {
		t.Fatalf("case = %q/%q/%q/%q/%q/%q", source, studyUID, seriesUID, orthancID, frameUID, status)
	}

	if got := countRows(t, ctx, pool, `SELECT count(*) FROM ai_jobs WHERE case_id = $1`, caseID); got != 1 {
		t.Fatalf("jobs = %d, want 1", got)
	}
	var jobStatus string
	var result, started, finished *string
	if err := pool.QueryRow(ctx,
		`SELECT status, result::text, started_at::text, finished_at::text FROM ai_jobs WHERE case_id = $1`, caseID).
		Scan(&jobStatus, &result, &started, &finished); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if jobStatus != "queued" || result != nil || started != nil || finished != nil {
		t.Fatalf("job = %q result=%v started=%v finished=%v", jobStatus, result, started, finished)
	}

	if got := countRows(t, ctx, pool, `SELECT count(*) FROM events WHERE case_id = $1`, caseID); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
	var actor, action string
	var detail []byte
	if err := pool.QueryRow(ctx,
		`SELECT actor, action, detail FROM events WHERE case_id = $1`, caseID).Scan(&actor, &action, &detail); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if actor != "importer" || action != "case_imported" {
		t.Fatalf("event = %q/%q", actor, action)
	}
	var decoded struct {
		InstanceCount int `json:"instance_count"`
	}
	if err := json.Unmarshal(detail, &decoded); err != nil {
		t.Fatalf("event detail: %v", err)
	}
	if decoded.InstanceCount != input.InstanceCount {
		t.Fatalf("event instance_count = %d", decoded.InstanceCount)
	}

	for _, table := range []string{"reads", "marks", "candidate_decisions"} {
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM `+table); got != 0 {
			t.Fatalf("%s rows = %d, want 0", table, got)
		}
	}
}

func TestImportStoreDuplicate(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	pool := importRuntimePool(t, h)
	store := postgres.NewImportStore(pool)
	ctx := context.Background()

	first := importInput("1.2.3.4.5.200", "1.2.3.4.5.99", "LIDC-IDRI")
	caseID, err := store.CreateImportedCase(ctx, first)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}

	second := importInput("1.2.3.4.5.200", "1.2.3.4.5.99", "local")
	_, err = store.CreateImportedCase(ctx, second)
	if !errors.Is(err, caseimport.ErrDuplicateSeries) {
		t.Fatalf("second import error = %v, want %v", err, caseimport.ErrDuplicateSeries)
	}

	if got := countRows(t, ctx, pool, `SELECT count(*) FROM cases`); got != 1 {
		t.Fatalf("cases = %d, want 1", got)
	}
	if got := countRows(t, ctx, pool, `SELECT count(*) FROM ai_jobs`); got != 1 {
		t.Fatalf("jobs = %d, want 1", got)
	}
	if got := countRows(t, ctx, pool, `SELECT count(*) FROM events`); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
	var source string
	if err := pool.QueryRow(ctx, `SELECT source FROM cases WHERE id = $1`, caseID).Scan(&source); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if source != "LIDC-IDRI" {
		t.Fatalf("source = %q, want the original", source)
	}
}

func TestImportStoreConcurrent(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	pool := importRuntimePool(t, h)
	store := postgres.NewImportStore(pool)
	ctx := context.Background()

	input := importInput("1.2.3.4.5.300", "1.2.3.4.5.99", "LIDC-IDRI")

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, results[index] = store.CreateImportedCase(ctx, input)
		}(i)
	}
	wg.Wait()

	successes, duplicates := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, caseimport.ErrDuplicateSeries):
			duplicates++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("successes = %d duplicates = %d, want 1/1", successes, duplicates)
	}
	for table, want := range map[string]int{"cases": 1, "ai_jobs": 1, "events": 1} {
		if got := countRows(t, ctx, pool, `SELECT count(*) FROM `+table); got != want {
			t.Fatalf("%s rows = %d, want %d", table, got, want)
		}
	}
}

func TestImportStoreRollback(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	pool := importRuntimePool(t, h)
	store := postgres.NewImportStore(pool)
	ctx := context.Background()
	owner := h.owner(t)

	fixtures := []struct {
		name      string
		seriesUID string
		trigger   string
	}{
		{
			name:      "job_insert",
			seriesUID: "1.2.3.4.5.400",
			trigger:   `CREATE TRIGGER lc1_08_fail_rows BEFORE INSERT ON ai_jobs FOR EACH ROW EXECUTE FUNCTION lc1_08_fail_rows()`,
		},
		{
			name:      "event_insert",
			seriesUID: "1.2.3.4.5.500",
			trigger:   `CREATE TRIGGER lc1_08_fail_rows BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION lc1_08_fail_rows()`,
		},
	}
	cleanup := func() {
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS lc1_08_fail_rows ON ai_jobs`,
			`DROP TRIGGER IF EXISTS lc1_08_fail_rows ON events`,
			`DROP FUNCTION IF EXISTS lc1_08_fail_rows()`,
		} {
			if _, err := owner.Exec(context.Background(), statement); err != nil {
				t.Fatalf("cleanup %q: %v", statement, err)
			}
		}
	}
	t.Cleanup(cleanup)

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			cleanup()
			if _, err := owner.Exec(ctx,
				`CREATE OR REPLACE FUNCTION lc1_08_fail_rows() RETURNS trigger LANGUAGE plpgsql AS $fn$ BEGIN RAISE EXCEPTION 'lc1-08 fixture'; END $fn$`); err != nil {
				t.Fatalf("create fixture function: %v", err)
			}
			if _, err := owner.Exec(ctx, fixture.trigger); err != nil {
				t.Fatalf("create fixture: %v", err)
			}
			if _, err := owner.Exec(ctx, `GRANT EXECUTE ON FUNCTION lc1_08_fail_rows() TO `+h.runtimeRole); err != nil {
				t.Fatalf("grant fixture: %v", err)
			}

			input := importInput(fixture.seriesUID, "1.2.3.4.5.99", "local")
			_, err := store.CreateImportedCase(ctx, input)
			if !errors.Is(err, caseimport.ErrDependency) {
				t.Fatalf("error = %v, want %v", err, caseimport.ErrDependency)
			}

			if got := countRows(t, ctx, pool, `SELECT count(*) FROM cases WHERE series_instance_uid = $1`, input.SeriesInstanceUID); got != 0 {
				t.Fatalf("case rows = %d, want 0", got)
			}
			if got := countRows(t, ctx, pool, `SELECT count(*) FROM ai_jobs`); got != 0 {
				t.Fatalf("job rows = %d, want 0", got)
			}
			if got := countRows(t, ctx, pool, `SELECT count(*) FROM events`); got != 0 {
				t.Fatalf("event rows = %d, want 0", got)
			}
		})
	}
}

func TestImportStoreCanceled(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	pool := importRuntimePool(t, h)
	store := postgres.NewImportStore(pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.CreateImportedCase(ctx, importInput("1.2.3.4.5.600", "1.2.3.4.5.99", "local"))
	if !errors.Is(err, caseimport.ErrCanceled) {
		t.Fatalf("error = %v, want %v", err, caseimport.ErrCanceled)
	}
	if got := countRows(t, context.Background(), pool, `SELECT count(*) FROM cases`); got != 0 {
		t.Fatalf("cases = %d, want 0", got)
	}
}

// TestImportStoreCommitOutcome covers failures raised while the transaction is
// being committed. A server-side rejection means a definite rollback; a lost
// connection leaves the outcome unknown and must not be reported as a
// completed rollback or as a plain cancellation.
func TestImportStoreCommitOutcome(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	pool := importRuntimePool(t, h)
	store := postgres.NewImportStore(pool)
	ctx := context.Background()
	owner := h.owner(t)

	fixtures := []struct {
		name      string
		seriesUID string
		body      string
		wantErr   error
	}{
		{
			name:      "server_rejected",
			seriesUID: "1.2.3.4.5.700",
			body:      `RAISE EXCEPTION 'lc1-08 commit fixture'`,
			wantErr:   caseimport.ErrDependency,
		},
		{
			name:      "connection_lost",
			seriesUID: "1.2.3.4.5.800",
			body:      `PERFORM pg_terminate_backend(pg_backend_pid())`,
			wantErr:   caseimport.ErrCommitOutcomeUnknown,
		},
	}
	cleanup := func() {
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS lc1_08_commit_fixture ON cases`,
			`DROP FUNCTION IF EXISTS lc1_08_commit_fixture()`,
		} {
			if _, err := owner.Exec(context.Background(), statement); err != nil {
				t.Fatalf("cleanup %q: %v", statement, err)
			}
		}
	}
	t.Cleanup(cleanup)

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			cleanup()
			if _, err := owner.Exec(ctx,
				`CREATE OR REPLACE FUNCTION lc1_08_commit_fixture() RETURNS trigger LANGUAGE plpgsql AS $fn$ BEGIN `+fixture.body+`; RETURN NULL; END $fn$`); err != nil {
				t.Fatalf("create fixture function: %v", err)
			}
			if _, err := owner.Exec(ctx,
				`CREATE CONSTRAINT TRIGGER lc1_08_commit_fixture AFTER INSERT ON cases DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION lc1_08_commit_fixture()`); err != nil {
				t.Fatalf("create fixture trigger: %v", err)
			}
			if _, err := owner.Exec(ctx, `GRANT EXECUTE ON FUNCTION lc1_08_commit_fixture() TO `+h.runtimeRole); err != nil {
				t.Fatalf("grant fixture: %v", err)
			}

			input := importInput(fixture.seriesUID, "1.2.3.4.5.99", "local")
			_, err := store.CreateImportedCase(ctx, input)
			if !errors.Is(err, fixture.wantErr) {
				t.Fatalf("error = %v, want %v", err, fixture.wantErr)
			}

			if got := countRows(t, ctx, pool, `SELECT count(*) FROM cases WHERE series_instance_uid = $1`, input.SeriesInstanceUID); got != 0 {
				t.Fatalf("case rows = %d, want 0", got)
			}
			if got := countRows(t, ctx, pool, `SELECT count(*) FROM ai_jobs`); got != 0 {
				t.Fatalf("job rows = %d, want 0", got)
			}
			if got := countRows(t, ctx, pool, `SELECT count(*) FROM events`); got != 0 {
				t.Fatalf("event rows = %d, want 0", got)
			}
		})
	}
}
