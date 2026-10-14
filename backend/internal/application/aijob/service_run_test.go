package aijob

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// runWithCancel runs the worker until the injected wait hook cancels it, so the
// lifetime loop can be tested without a real timer.
func runWithCancel(t *testing.T, hook func(*fakeStore, int) bool) (*fakeStore, *fakeRunner, error) {
	t.Helper()
	store := newFakeStore()
	runner := &fakeRunner{}
	service := NewService(store, noArchive{}, runner, 15*time.Minute)
	service.interval = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	steps := 0
	service.wait = func(context.Context, time.Duration) {
		steps++
		if hook(store, steps) {
			cancel()
		}
	}
	return store, runner, service.Run(ctx)
}

func TestRunPicksUpJobQueuedAfterStart(t *testing.T) {
	store, runner, err := runWithCancel(t, func(store *fakeStore, step int) bool {
		if step == 1 {
			store.addJob(testServiceJob(7), "queued", true)
		}
		return step >= 3
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0] != 7 {
		t.Fatalf("runner calls = %v, want exactly job 7", runner.calls)
	}
	if state := store.jobs[7]; state.status != "succeeded" || !state.hasResult {
		t.Fatalf("late job state = %+v", state)
	}
}

func TestRunSurvivesTransientListFailure(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(3), "queued", true)
	store.listFails = 1
	runner := &fakeRunner{}
	service := NewService(store, noArchive{}, runner, 15*time.Minute)
	service.interval = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	steps := 0
	service.wait = func(context.Context, time.Duration) {
		steps++
		if steps >= 2 {
			cancel()
		}
	}

	if err := service.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if state := store.jobs[3]; state.status != "succeeded" {
		t.Fatalf("job after a transient list failure = %+v", state)
	}
}

func TestRunNeverRunsJobsConcurrently(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(1), "queued", true)
	store.addJob(testServiceJob(2), "queued", true)
	var active, maxActive int32
	runner := &fakeRunner{fn: func(job Job) (Execution, error) {
		current := atomic.AddInt32(&active, 1)
		for {
			peak := atomic.LoadInt32(&maxActive)
			if current <= peak || atomic.CompareAndSwapInt32(&maxActive, peak, current) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return Execution{ExitCode: 0, Result: successRaw(job)}, nil
	}}
	service := NewService(store, noArchive{}, runner, 15*time.Minute)
	service.interval = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	steps := 0
	service.wait = func(context.Context, time.Duration) {
		steps++
		if steps >= 2 {
			cancel()
		}
	}

	if err := service.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls = %v, want both jobs", runner.calls)
	}
	if maxActive != 1 {
		t.Fatalf("jobs ran concurrently: peak = %d", maxActive)
	}
}

func TestRunBoundsEachBatch(t *testing.T) {
	store := newFakeStore()
	for id := int64(1); id <= 100; id++ {
		store.addJob(testServiceJob(id), "queued", true)
	}
	runner := &fakeRunner{}
	service := NewService(store, noArchive{}, runner, 15*time.Minute)
	service.interval = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.wait = func(context.Context, time.Duration) { cancel() }

	if err := service.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if store.claimCalls != batchSize {
		t.Fatalf("claimed %d jobs in one batch, want %d", store.claimCalls, batchSize)
	}
}

func TestRunDoesNotRerunTerminalJobs(t *testing.T) {
	store := newFakeStore()
	store.addJob(testServiceJob(1), "succeeded", true)
	store.addJob(testServiceJob(2), "failed", true)
	runner := &fakeRunner{}
	service := NewService(store, noArchive{}, runner, 15*time.Minute)
	service.interval = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.wait = func(context.Context, time.Duration) { cancel() }

	if err := service.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("terminal jobs were re-run: %v", runner.calls)
	}
	if store.succeedCalls != 0 || store.failCalls != 0 {
		t.Fatalf("terminal writes = succeed %d fail %d", store.succeedCalls, store.failCalls)
	}
	if store.jobs[1].status != "succeeded" || store.jobs[2].status != "failed" {
		t.Fatalf("terminal states changed: %+v %+v", store.jobs[1], store.jobs[2])
	}
}

func TestRunStopsPromptlyWhenCanceled(t *testing.T) {
	store := newFakeStore()
	runner := &fakeRunner{}
	service := NewService(store, noArchive{}, runner, 15*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := service.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if store.listCalls != 0 {
		t.Fatalf("Run listed jobs %d times after cancellation", store.listCalls)
	}
}

var _ = errors.Is
