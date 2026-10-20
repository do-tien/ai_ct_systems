package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/aijob"
	"nodule-reader/backend/internal/infrastructure/postgres"
)

func jobStore(t *testing.T, h *storageHarness) (*postgres.AIJobStore, *pgxpool.Pool) {
	t.Helper()
	pool := importRuntimePool(t, h)
	return postgres.NewAIJobStore(pool), pool
}

// seedQueuedJob inserts one case and one queued job and returns their ids.
func seedQueuedJob(t *testing.T, pool *pgxpool.Pool, seriesUID, caseStatus string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	caseID := seedCase(t, pool, seriesUID, caseStatus)
	var jobID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO ai_jobs (case_id, status) VALUES ($1, 'queued') RETURNING id`, caseID).Scan(&jobID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return caseID, jobID
}

func jobStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM ai_jobs WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read job status: %v", err)
	}
	return status
}

// aiJobStubRunner is a test-only aijob.Runner that returns one fixed execution
// without retrieving or inferring anything. It drives the real use case's
// validation boundary against real job persistence.
type aiJobStubRunner struct {
	execution aijob.Execution
}

func (r *aiJobStubRunner) Run(context.Context, aijob.Job, aijob.Archive) (aijob.Execution, error) {
	return r.execution, nil
}

// aiJobRefusingArchive is a test-only aijob.Archive that fails the test if the
// use case itself ever downloads a series. The store adapter owns persistence
// and the runner owns retrieval, so passing this archive documents that a
// future regression moving a download into the use case would be caught; in
// this test the stub runner never touches the archive and no download occurs.
type aiJobRefusingArchive struct{ t *testing.T }

func (a aiJobRefusingArchive) Download(_ context.Context, seriesID string, _ io.Writer) error {
	a.t.Errorf("archive.Download(%q) called by the use case; retrieval belongs to the runner", seriesID)
	return errors.New("unexpected archive download")
}

// TestAIJobServiceRejectsInvalidGeometry drives the real LC1-12 use case and
// real AI-job persistence: a complete, otherwise-valid success document that
// mutates exactly one linkage or coordinate field must persist as a
// RESULT_INVALID failure with no success JSONB and no physician-data change,
// while an otherwise-identical valid control persists. The runner never
// downloads images or invokes Python.
func TestAIJobServiceRejectsInvalidGeometry(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := jobStore(t, h)
	ctx := context.Background()

	documentWith := func(t *testing.T, series, frame string, candidates []string, mutate func(map[string]any)) json.RawMessage {
		t.Helper()
		var document map[string]any
		if err := json.Unmarshal(syntheticAISuccess(t, series, frame, candidates...), &document); err != nil {
			t.Fatalf("decode synthetic result: %v", err)
		}
		if mutate != nil {
			mutate(document)
		}
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("encode mutated result: %v", err)
		}
		return raw
	}

	cases := []struct {
		name       string
		series     string
		frame      string
		candidates []string
		mutate     func(map[string]any)
		wantStatus string
	}{
		{"valid_control", "1.2.3.4.5.700", "1.2.3.4.7", nil, nil, "succeeded"},
		{"coordinate_system_ras", "1.2.3.4.5.701", "1.2.3.4.7", []string{"c1"}, func(d map[string]any) {
			d["coordinate_system"] = "RAS_mm"
		}, "failed"},
		{"series_instance_uid_mismatch", "1.2.3.4.5.702", "1.2.3.4.7", []string{"c1"}, func(d map[string]any) {
			d["series_instance_uid"] = "1.2.3.4.5.7999"
		}, "failed"},
		{"frame_of_reference_uid_mismatch", "1.2.3.4.5.703", "1.2.3.4.7", []string{"c1"}, func(d map[string]any) {
			d["frame_of_reference_uid"] = "1.2.3.4.8"
		}, "failed"},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, jobID := seedQueuedJob(t, pool, test.series, "awaiting_read")
			result := documentWith(t, test.series, test.frame, test.candidates, test.mutate)
			before := snapshotClinical(t, pool)

			service := aijob.NewService(store, aiJobRefusingArchive{t}, &aiJobStubRunner{
				execution: aijob.Execution{ExitCode: 0, Result: result},
			}, time.Second)
			if err := service.Drain(ctx); err != nil {
				t.Fatalf("Drain: %v", err)
			}

			if after := snapshotClinical(t, pool); before != after {
				t.Fatalf("physician-owned data changed by the drain:\nbefore %s\nafter  %s", before, after)
			}

			var (
				status       string
				storedResult []byte
				errorCode    *string
				finishedSet  bool
			)
			if err := pool.QueryRow(ctx,
				`SELECT status, result, error_code, finished_at IS NOT NULL FROM ai_jobs WHERE id = $1`, jobID).Scan(
				&status, &storedResult, &errorCode, &finishedSet); err != nil {
				t.Fatalf("read job: %v", err)
			}
			if !finishedSet {
				t.Fatalf("%s: finished_at not populated", test.name)
			}

			if test.wantStatus == "succeeded" {
				if status != "succeeded" || errorCode != nil {
					t.Fatalf("valid control stored status %q code %v, want succeeded/nil", status, errorCode)
				}
				var storedDoc, wantDoc any
				if err := json.Unmarshal(storedResult, &storedDoc); err != nil {
					t.Fatalf("decode stored result: %v", err)
				}
				if err := json.Unmarshal(result, &wantDoc); err != nil {
					t.Fatalf("decode expected result: %v", err)
				}
				if !reflect.DeepEqual(storedDoc, wantDoc) {
					t.Fatalf("valid control stored result = %s, want %s", storedResult, result)
				}
				return
			}

			if status != "failed" || errorCode == nil || *errorCode != aijob.CodeResultInvalid {
				t.Fatalf("stored status %q code %v, want failed/%s", status, errorCode, aijob.CodeResultInvalid)
			}
			if storedResult != nil {
				t.Fatalf("invalid geometry persisted a result document: %s", storedResult)
			}
		})
	}
}

func TestAIJobListQueuedIsOrderedAndExcludesRunning(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := jobStore(t, h)
	ctx := context.Background()

	_, first := seedQueuedJob(t, pool, "1.2.3.4.5.201", "awaiting_read")
	_, second := seedQueuedJob(t, pool, "1.2.3.4.5.202", "awaiting_read")
	_, third := seedQueuedJob(t, pool, "1.2.3.4.5.203", "awaiting_read")

	if _, claimed, err := store.Claim(ctx, first); err != nil || !claimed {
		t.Fatalf("Claim(first) = claimed %v err %v", claimed, err)
	}

	ids, err := store.ListQueued(ctx)
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	if !reflect.DeepEqual(ids, []int64{second, third}) {
		t.Fatalf("ListQueued() = %v, want [%d %d]", ids, second, third)
	}
}

func TestAIJobClaimSingleWinner(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := jobStore(t, h)
	ctx := context.Background()

	caseID, jobID := seedQueuedJob(t, pool, "1.2.3.4.5.210", "awaiting_read")

	const claimers = 4
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]struct {
		job     aijob.Job
		claimed bool
		err     error
	}, claimers)
	wg.Add(claimers)
	for index := 0; index < claimers; index++ {
		go func(index int) {
			defer wg.Done()
			<-start
			results[index].job, results[index].claimed, results[index].err = store.Claim(ctx, jobID)
		}(index)
	}
	close(start)
	wg.Wait()

	winners := 0
	for index, result := range results {
		if result.err != nil {
			t.Fatalf("claimer %d error: %v", index, result.err)
		}
		if result.claimed {
			winners++
			if result.job.CaseID != caseID || result.job.ID != jobID {
				t.Fatalf("winner job = %+v", result.job)
			}
			if result.job.OrthancSeriesID != "orthanc-1" || result.job.SeriesInstanceUID != "1.2.3.4.5.210" {
				t.Fatalf("winner linkage = %+v", result.job)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if status := jobStatus(t, pool, jobID); status != "running" {
		t.Fatalf("status = %s, want running", status)
	}
	var startedValid bool
	if err := pool.QueryRow(ctx, `SELECT started_at IS NOT NULL FROM ai_jobs WHERE id = $1`, jobID).Scan(&startedValid); err != nil {
		t.Fatalf("started_at query: %v", err)
	}
	if !startedValid {
		t.Fatal("started_at was not set on the claimed job")
	}
}

func TestAIJobClaimRejectsNonQueued(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := jobStore(t, h)
	ctx := context.Background()

	_, running := seedQueuedJob(t, pool, "1.2.3.4.5.220", "awaiting_read")
	if _, claimed, err := store.Claim(ctx, running); err != nil || !claimed {
		t.Fatalf("first Claim = claimed %v err %v", claimed, err)
	}
	if _, claimed, err := store.Claim(ctx, running); err != nil || claimed {
		t.Fatalf("second Claim = claimed %v err %v, want not claimed", claimed, err)
	}

	_, succeeded := seedQueuedJob(t, pool, "1.2.3.4.5.221", "awaiting_read")
	if _, claimed, err := store.Claim(ctx, succeeded); err != nil || !claimed {
		t.Fatalf("Claim(succeeded job) = claimed %v err %v", claimed, err)
	}
	if err := store.Succeed(ctx, succeeded, json.RawMessage(`{"schema_version":"1"}`)); err != nil {
		t.Fatalf("Succeed: %v", err)
	}
	if _, claimed, err := store.Claim(ctx, succeeded); err != nil || claimed {
		t.Fatalf("Claim(terminal job) = claimed %v err %v, want not claimed", claimed, err)
	}

	if _, claimed, err := store.Claim(ctx, 1<<40); err != nil || claimed {
		t.Fatalf("Claim(missing) = claimed %v err %v, want not claimed, no error", claimed, err)
	}
}

func TestAIJobClaimStatementFailureLeavesQueued(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := jobStore(t, h)
	owner := h.owner(t)
	ctx := context.Background()

	_, jobID := seedQueuedJob(t, pool, "1.2.3.4.5.230", "awaiting_read")

	// Hide the linkage table so the claim's metadata read fails after the
	// conditional start has already prepared. The transaction must roll back
	// and leave the job queued.
	if _, err := owner.Exec(ctx, `ALTER TABLE cases RENAME TO cases_hidden`); err != nil {
		t.Fatalf("rename cases: %v", err)
	}
	deferred := true
	defer func() {
		if deferred {
			if _, err := owner.Exec(context.Background(), `ALTER TABLE cases_hidden RENAME TO cases`); err != nil {
				t.Errorf("restore cases: %v", err)
			}
		}
	}()

	if _, claimed, err := store.Claim(ctx, jobID); err == nil || claimed {
		t.Fatalf("Claim during statement failure = claimed %v err %v, want error", claimed, err)
	} else if !errors.Is(err, aijob.ErrDependency) {
		t.Fatalf("Claim error = %v, want ErrDependency", err)
	}

	if _, err := owner.Exec(ctx, `ALTER TABLE cases_hidden RENAME TO cases`); err != nil {
		t.Fatalf("restore cases: %v", err)
	}
	deferred = false

	if status := jobStatus(t, pool, jobID); status != "queued" {
		t.Fatalf("status = %s, want queued after a failed claim", status)
	}
}

func TestAIJobTerminalFieldsExact(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := jobStore(t, h)
	ctx := context.Background()

	_, succeeded := seedQueuedJob(t, pool, "1.2.3.4.5.240", "awaiting_read")
	if _, claimed, err := store.Claim(ctx, succeeded); err != nil || !claimed {
		t.Fatalf("Claim: %v", err)
	}
	result := json.RawMessage(`{"schema_version":"1","candidates":[]}`)
	if err := store.Succeed(ctx, succeeded, result); err != nil {
		t.Fatalf("Succeed: %v", err)
	}
	if err := store.Succeed(ctx, succeeded, result); !errors.Is(err, aijob.ErrConflict) {
		t.Fatalf("second Succeed = %v, want ErrConflict", err)
	}
	if err := store.Fail(ctx, succeeded, aijob.Failure{Code: aijob.CodeProcessError}); !errors.Is(err, aijob.ErrConflict) {
		t.Fatalf("Fail on succeeded = %v, want ErrConflict", err)
	}

	var (
		status                  string
		storedResult            []byte
		errorCode, stderrTail   *string
		startedSet, finishedSet bool
	)
	if err := pool.QueryRow(ctx,
		`SELECT status, result, error_code, stderr_tail,
		        started_at IS NOT NULL, finished_at IS NOT NULL
		 FROM ai_jobs WHERE id = $1`, succeeded).Scan(
		&status, &storedResult, &errorCode, &stderrTail, &startedSet, &finishedSet); err != nil {
		t.Fatalf("read succeeded job: %v", err)
	}
	// JSONB normalises formatting and key order, so compare the decoded value.
	var storedDoc, wantDoc any
	if err := json.Unmarshal(storedResult, &storedDoc); err != nil {
		t.Fatalf("decode stored result: %v", err)
	}
	if err := json.Unmarshal(result, &wantDoc); err != nil {
		t.Fatalf("decode expected result: %v", err)
	}
	if status != "succeeded" || !reflect.DeepEqual(storedDoc, wantDoc) || errorCode != nil || stderrTail != nil || !startedSet || !finishedSet {
		t.Fatalf("succeeded job = status %s result %s code %v tail %v started %v finished %v",
			status, storedResult, errorCode, stderrTail, startedSet, finishedSet)
	}

	_, failed := seedQueuedJob(t, pool, "1.2.3.4.5.241", "awaiting_read")
	if _, claimed, err := store.Claim(ctx, failed); err != nil || !claimed {
		t.Fatalf("Claim(failed job): %v", err)
	}
	if err := store.Fail(ctx, failed, aijob.Failure{Code: aijob.CodeAITimeout, StderrTail: "[redacted]\ninfer: MODEL_OOM"}); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if err := store.Fail(ctx, failed, aijob.Failure{Code: aijob.CodeProcessError}); !errors.Is(err, aijob.ErrConflict) {
		t.Fatalf("second Fail = %v, want ErrConflict", err)
	}
	if err := store.Succeed(ctx, failed, result); !errors.Is(err, aijob.ErrConflict) {
		t.Fatalf("Succeed on failed = %v, want ErrConflict", err)
	}

	var (
		failStatus  string
		failResult  []byte
		failCode    *string
		failTail    *string
		failStarted bool
		failDone    bool
	)
	if err := pool.QueryRow(ctx,
		`SELECT status, result, error_code, stderr_tail,
		        started_at IS NOT NULL, finished_at IS NOT NULL
		 FROM ai_jobs WHERE id = $1`, failed).Scan(
		&failStatus, &failResult, &failCode, &failTail, &failStarted, &failDone); err != nil {
		t.Fatalf("read failed job: %v", err)
	}
	if failStatus != "failed" || failResult != nil || failCode == nil || *failCode != aijob.CodeAITimeout ||
		failTail == nil || *failTail != "[redacted]\ninfer: MODEL_OOM" || !failStarted || !failDone {
		t.Fatalf("failed job = status %s result %v code %v tail %v started %v finished %v",
			failStatus, failResult, failCode, failTail, failStarted, failDone)
	}

	// A never-claimed queued job cannot be terminated.
	_, queued := seedQueuedJob(t, pool, "1.2.3.4.5.242", "awaiting_read")
	if err := store.Succeed(ctx, queued, result); !errors.Is(err, aijob.ErrConflict) {
		t.Fatalf("Succeed on queued = %v, want ErrConflict", err)
	}
	if err := store.Fail(ctx, queued, aijob.Failure{Code: aijob.CodeProcessError}); !errors.Is(err, aijob.ErrConflict) {
		t.Fatalf("Fail on queued = %v, want ErrConflict", err)
	}
}

func TestAIJobTerminalWritesLeavePhysicianDataUnchanged(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	store, pool := jobStore(t, h)
	ctx := context.Background()

	ownerID := readerUserID(t, pool)
	caseID := seedCase(t, pool, "1.2.3.4.5.250", "awaiting_read")
	var initialRead, finalRead int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO reads (case_id, kind, reader_user_id) VALUES ($1, 'initial', $2) RETURNING id`,
		caseID, ownerID).Scan(&initialRead); err != nil {
		t.Fatalf("seed initial read: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO reads (case_id, kind, reader_user_id) VALUES ($1, 'final', $2) RETURNING id`,
		caseID, ownerID).Scan(&finalRead); err != nil {
		t.Fatalf("seed final read: %v", err)
	}
	initialMark, initialMarkArgs := markInsert(initialRead, validTripleA, validTripleB, validCenter, "17.0", "'reader'", "NULL", "'{0,0,1}'::float8[]")
	if _, err := pool.Exec(ctx, initialMark, initialMarkArgs...); err != nil {
		t.Fatalf("seed initial mark: %v", err)
	}
	finalMark, finalMarkArgs := markInsert(finalRead, validTripleA, validTripleB, validCenter, "17.0", "'reader'", "NULL", "'{0,0,1}'::float8[]")
	if _, err := pool.Exec(ctx, finalMark, finalMarkArgs...); err != nil {
		t.Fatalf("seed final mark: %v", err)
	}
	// Lock the initial read only after its marks exist: the immutability guard
	// rejects mark inserts on a locked read.
	if _, err := pool.Exec(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1`, initialRead); err != nil {
		t.Fatalf("lock initial read: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO candidate_decisions (read_id, candidate_id, decision, dismiss_reason)
		 VALUES ($1, 'c1', 'dismissed', 'vessel, not a nodule')`, finalRead); err != nil {
		t.Fatalf("seed decision: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE cases SET status = 'finalized', finalized_at = now() WHERE id = $1`, caseID); err != nil {
		t.Fatalf("finalize case: %v", err)
	}

	before := snapshotClinical(t, pool)

	// Both jobs belong to the finalized case; only ai_jobs may change.
	var succeeded, failed int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO ai_jobs (case_id, status) VALUES ($1, 'queued') RETURNING id`, caseID).Scan(&succeeded); err != nil {
		t.Fatalf("seed succeeded job: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO ai_jobs (case_id, status) VALUES ($1, 'queued') RETURNING id`, caseID).Scan(&failed); err != nil {
		t.Fatalf("seed failed job: %v", err)
	}
	if _, claimed, err := store.Claim(ctx, succeeded); err != nil || !claimed {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Succeed(ctx, succeeded, json.RawMessage(`{"schema_version":"1","candidates":[]}`)); err != nil {
		t.Fatalf("Succeed after finalized: %v", err)
	}
	if _, claimed, err := store.Claim(ctx, failed); err != nil || !claimed {
		t.Fatalf("Claim: %v", err)
	}
	if err := store.Fail(ctx, failed, aijob.Failure{Code: aijob.CodeProcessError}); err != nil {
		t.Fatalf("Fail after finalized: %v", err)
	}

	after := snapshotClinical(t, pool)
	if before != after {
		t.Fatalf("physician-owned data changed by a job terminal write:\nbefore %s\nafter  %s", before, after)
	}
}

// snapshotClinical renders every physician-owned table (cases, reads, marks,
// candidate_decisions) as canonical JSON so any change is detectable. ai_jobs
// is deliberately excluded.
func snapshotClinical(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot string
	if err := pool.QueryRow(context.Background(), `
		SELECT
		  COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.id) FROM cases x), '[]'::jsonb)::text || '|' ||
		  COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.id) FROM reads x), '[]'::jsonb)::text || '|' ||
		  COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.id) FROM marks x), '[]'::jsonb)::text || '|' ||
		  COALESCE((SELECT jsonb_agg(to_jsonb(x) ORDER BY x.read_id, x.candidate_id) FROM candidate_decisions x), '[]'::jsonb)::text
	`).Scan(&snapshot); err != nil {
		t.Fatalf("snapshot clinical data: %v", err)
	}
	return snapshot
}
