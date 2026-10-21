package aijob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeStore models the ai_jobs table and its conditional transitions so tests
// assert observable state, not calls echoed back by a mock.
type fakeStore struct {
	queued  []int64
	jobs    map[int64]*fakeJobState
	listErr error
	// listFails makes the next N ListQueued calls fail before listErr applies.
	listFails  int
	claimErr   error
	succeedErr error
	failErr    error

	listCalls    int
	claimCalls   int
	succeedCalls int
	failCalls    int
}

type fakeJobState struct {
	job       Job
	status    string
	result    json.RawMessage
	failure   Failure
	hasResult bool
	hasFailed bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{jobs: map[int64]*fakeJobState{}}
}

func (s *fakeStore) addJob(job Job, status string, inSnapshot bool) {
	s.jobs[job.ID] = &fakeJobState{job: job, status: status}
	if inSnapshot {
		s.queued = append(s.queued, job.ID)
	}
}

func (s *fakeStore) ListQueued(context.Context) ([]int64, error) {
	s.listCalls++
	if s.listFails > 0 {
		s.listFails--
		return nil, errors.New("scripted transient list failure")
	}
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]int64(nil), s.queued...), nil
}

func (s *fakeStore) Claim(_ context.Context, id int64) (Job, bool, error) {
	s.claimCalls++
	if s.claimErr != nil {
		return Job{}, false, s.claimErr
	}
	state, ok := s.jobs[id]
	if !ok || state.status != "queued" {
		return Job{}, false, nil
	}
	state.status = "running"
	return state.job, true, nil
}

func (s *fakeStore) Succeed(_ context.Context, id int64, result json.RawMessage) error {
	s.succeedCalls++
	if s.succeedErr != nil {
		return s.succeedErr
	}
	state := s.jobs[id]
	if state == nil || state.status != "running" {
		return ErrConflict
	}
	state.status = "succeeded"
	state.result = result
	state.hasResult = true
	return nil
}

func (s *fakeStore) Fail(_ context.Context, id int64, failure Failure) error {
	s.failCalls++
	if s.failErr != nil {
		return s.failErr
	}
	state := s.jobs[id]
	if state == nil || state.status != "running" {
		return ErrConflict
	}
	state.status = "failed"
	state.failure = failure
	state.hasFailed = true
	return nil
}

type fakeRunner struct {
	calls []int64
	fn    func(job Job) (Execution, error)
}

func (r *fakeRunner) Run(_ context.Context, job Job, _ Archive) (Execution, error) {
	r.calls = append(r.calls, job.ID)
	if r.fn != nil {
		return r.fn(job)
	}
	return Execution{ExitCode: 0, Result: successRaw(job)}, nil
}

type noArchive struct{}

func (noArchive) Download(context.Context, string, io.Writer) error { return nil }

func successRaw(job Job) json.RawMessage {
	doc := validSuccessDocument()
	doc["series_instance_uid"] = job.SeriesInstanceUID
	doc["frame_of_reference_uid"] = job.FrameOfReferenceUID
	raw, _ := json.Marshal(doc)
	return raw
}

func errorRaw(code string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"schema_version": "1",
		"error":          map[string]any{"code": code, "message": "safe"},
	})
	return raw
}

func testServiceJob(id int64) Job {
	uid := testSeriesUID[:len(testSeriesUID)-1]
	return Job{
		ID:                  id,
		CaseID:              100 + id,
		OrthancSeriesID:     "11111111-22222222-33333333-44444444-55555555",
		SeriesInstanceUID:   uid + "0",
		FrameOfReferenceUID: testFrameUID,
	}
}

func runService(store Store, runner Runner) error {
	service := NewService(store, noArchive{}, runner, 15*time.Minute)
	return service.Drain(context.Background())
}

func TestServiceProcessesSnapshotJob(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(1), "queued", true)
	runner := &fakeRunner{}

	if err := runService(store, runner); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	state := store.jobs[1]
	if state.status != "succeeded" || !state.hasResult || !json.Valid(state.result) {
		t.Fatalf("job state = %+v", state)
	}
	if store.succeedCalls != 1 || store.failCalls != 0 || store.listCalls != 1 {
		t.Fatalf("store calls = succeed %d fail %d list %d", store.succeedCalls, store.failCalls, store.listCalls)
	}
	if len(runner.calls) != 1 || runner.calls[0] != 1 {
		t.Fatalf("runner calls = %v", runner.calls)
	}
}

func TestServiceSkipsNonQueuedJob(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(1), "running", true)
	runner := &fakeRunner{}

	if err := runService(store, runner); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner invoked for a non-queued job: %v", runner.calls)
	}
	if store.jobs[1].status != "running" {
		t.Fatalf("job state = %+v", store.jobs[1])
	}
}

func TestServiceInvalidResultNeverSucceeds(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(1), "queued", true)
	runner := &fakeRunner{fn: func(Job) (Execution, error) {
		return Execution{ExitCode: 0, Result: json.RawMessage("{not json")}, nil
	}}

	if err := runService(store, runner); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	state := store.jobs[1]
	if state.status != "failed" || state.failure.Code != CodeResultInvalid {
		t.Fatalf("job state = %+v", state)
	}
	if store.succeedCalls != 0 {
		t.Fatalf("Succeed was called %d times for an invalid result", store.succeedCalls)
	}
}

func TestServiceExitStatusPrecedesSuccessShapedFile(t *testing.T) {
	cases := map[string]struct {
		execution Execution
		code      string
	}{
		"exit2_with_success_file": {Execution{ExitCode: 2, Result: successRaw(testServiceJob(1))}, CodeResultInvalid},
		"exit2_valid_envelope":    {Execution{ExitCode: 2, Result: errorRaw("INVALID_DICOM"), StderrTail: "[redacted]"}, "INVALID_DICOM"},
		"exit3_missing_file":      {Execution{ExitCode: 3}, CodeResultMissing},
		"exit_other":              {Execution{ExitCode: 7, Result: successRaw(testServiceJob(1))}, CodeProcessError},
		"exit0_missing_file":      {Execution{ExitCode: 0}, CodeResultMissing},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore()
			store.addJob(testServiceJob(1), "queued", true)
			execution := testCase.execution
			runner := &fakeRunner{fn: func(Job) (Execution, error) { return execution, nil }}

			if err := runService(store, runner); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			state := store.jobs[1]
			if state.status != "failed" || state.failure.Code != testCase.code {
				t.Fatalf("job state = %+v, want code %s", state, testCase.code)
			}
			if store.succeedCalls != 0 {
				t.Fatalf("Succeed was called for a failing exit")
			}
		})
	}
}

func TestServiceMapsRunnerErrorCode(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(1), "queued", true)
	runner := &fakeRunner{fn: func(Job) (Execution, error) {
		return Execution{}, &CodeError{Code: CodeAITimeout, StderrTail: "infer: MODEL_OOM"}
	}}

	if err := runService(store, runner); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	state := store.jobs[1]
	if state.status != "failed" || state.failure.Code != CodeAITimeout || state.failure.StderrTail != "infer: MODEL_OOM" {
		t.Fatalf("job state = %+v", state)
	}
}

func TestServiceContinuesAfterJobFailure(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(1), "queued", true)
	store.addJob(testServiceJob(2), "queued", true)
	runner := &fakeRunner{fn: func(job Job) (Execution, error) {
		if job.ID == 1 {
			return Execution{ExitCode: 3, Result: errorRaw("MODEL_OOM")}, nil
		}
		return Execution{ExitCode: 0, Result: successRaw(job)}, nil
	}}

	if err := runService(store, runner); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if store.jobs[1].status != "failed" || store.jobs[1].failure.Code != "MODEL_OOM" {
		t.Fatalf("job 1 = %+v", store.jobs[1])
	}
	if store.jobs[2].status != "succeeded" {
		t.Fatalf("job 2 = %+v", store.jobs[2])
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls = %v", runner.calls)
	}
}

func TestServiceStopsOnUncertainClaim(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(1), "queued", true)
	store.claimErr = ErrOutcomeUnknown
	runner := &fakeRunner{}

	err := runService(store, runner)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Run() error = %v, want ErrOutcomeUnknown", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("runner invoked after an uncertain claim: %v", runner.calls)
	}
	if store.jobs[1].status != "queued" {
		t.Fatalf("job state = %+v", store.jobs[1])
	}
}

func TestServiceStopsOnTerminalWriteFailure(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(1), "queued", true)
	store.addJob(testServiceJob(2), "queued", true)
	store.failErr = ErrOutcomeUnknown
	runner := &fakeRunner{fn: func(Job) (Execution, error) {
		return Execution{ExitCode: 3, Result: errorRaw("MODEL_OOM")}, nil
	}}

	err := runService(store, runner)
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("Run() error = %v, want ErrOutcomeUnknown", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %v, want the drain to stop after one job", runner.calls)
	}
	if store.jobs[2].status != "queued" {
		t.Fatalf("job 2 = %+v, want it untouched", store.jobs[2])
	}
}

func TestServiceSnapshotIsReadOnce(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(1), "queued", true)
	// A queued job created after the snapshot must not be consumed.
	store.addJob(testServiceJob(2), "queued", false)
	runner := &fakeRunner{}

	if err := runService(store, runner); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if store.listCalls != 1 {
		t.Fatalf("ListQueued calls = %d, want 1", store.listCalls)
	}
	if store.jobs[2].status != "queued" {
		t.Fatalf("post-snapshot job was consumed: %+v", store.jobs[2])
	}
}

func TestServiceProcessesFinalizedCaseLinkage(t *testing.T) {
	store := newFakeStore()
	job := testServiceJob(1)
	store.addJob(job, "queued", true)
	runner := &fakeRunner{}

	if err := runService(store, runner); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if store.jobs[1].status != "succeeded" {
		t.Fatalf("finalized-case job = %+v", store.jobs[1])
	}
}

func TestServiceLogsLifecycleWithoutIdentifiers(t *testing.T) {
	var buffer bytes.Buffer
	previous := logLifecycle
	logLifecycle = func(format string, args ...any) { fmt.Fprintf(&buffer, format+"\n", args...) }
	defer func() { logLifecycle = previous }()

	job := testServiceJob(1)
	store := newFakeStore()
	store.addJob(job, "queued", true)
	runner := &fakeRunner{fn: func(Job) (Execution, error) {
		return Execution{ExitCode: 3, Result: errorRaw("BUNDLE_INVALID")}, nil
	}}
	if err := runService(store, runner); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	logged := buffer.String()
	for _, want := range []string{"ai job 1", "claimed", "BUNDLE_INVALID"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log = %q, want it to contain %q", logged, want)
		}
	}
	for _, secret := range []string{job.SeriesInstanceUID, job.OrthancSeriesID, "stderr", "{\"schema_version\""} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %q", secret, logged)
		}
	}
}

func TestServiceLogsSuccessOutcome(t *testing.T) {
	var buffer bytes.Buffer
	previous := logLifecycle
	logLifecycle = func(format string, args ...any) { fmt.Fprintf(&buffer, format+"\n", args...) }
	defer func() { logLifecycle = previous }()

	store := newFakeStore()
	store.addJob(testServiceJob(1), "queued", true)
	if err := runService(store, &fakeRunner{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(buffer.String(), "ai job 1: succeeded") {
		t.Fatalf("log = %q, want a success outcome", buffer.String())
	}
}
