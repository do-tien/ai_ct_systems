package aijob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

// persistenceTimeout bounds a detached terminal write so canceled work cannot
// prevent recording its outcome.
const persistenceTimeout = 5 * time.Second

// batchSize bounds how many queued jobs one scan handles.
const batchSize = 32

// defaultScanInterval is how long the worker waits before scanning again.
const defaultScanInterval = 2 * time.Second

// logLifecycle records one fixed lifecycle line per job. It carries the job ID,
// a fixed operational category and the outcome only: never a stderr tail, a
// result body, a path, a credential, a real UID, a patient tag or upstream
// error text. Tests replace it to keep their output quiet.
var logLifecycle = log.Printf

// Store is the consumer-owned persistence port for the AI job lifecycle.
type Store interface {
	// ListQueued returns the queued job IDs in ascending order. The worker
	// bounds each batch; it never marks anything running.
	ListQueued(ctx context.Context) ([]int64, error)
	// Claim conditionally transitions one queued job to running and returns its
	// immutable case linkage.
	Claim(ctx context.Context, id int64) (Job, bool, error)
	// Succeed records a fully validated result on a running job.
	Succeed(ctx context.Context, id int64, result json.RawMessage) error
	// Fail records a sanitized failure on a running job.
	Fail(ctx context.Context, id int64, failure Failure) error
}

// Runner executes one claimed job and returns its process outcome.
type Runner interface {
	Run(ctx context.Context, job Job, archive Archive) (Execution, error)
}

// Service is the LC1-12 use case: a lifetime worker that serially claims queued
// jobs, runs the inference CLI and persists only a validated result or a
// sanitized failure. It never writes reads, marks or case state.
type Service struct {
	store    Store
	archive  Archive
	runner   Runner
	timeout  time.Duration
	interval time.Duration
	wait     func(context.Context, time.Duration)
}

// NewService builds the use case from its ports and the per-job budget.
func NewService(store Store, archive Archive, runner Runner, timeout time.Duration) *Service {
	return &Service{
		store:    store,
		archive:  archive,
		runner:   runner,
		timeout:  timeout,
		interval: defaultScanInterval,
		wait:     waitForInterval,
	}
}

// waitForInterval blocks until the interval elapses or the context is canceled.
func waitForInterval(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Run owns the worker lifetime: it drains one bounded batch immediately and
// keeps draining newly queued jobs until the caller cancels the context. A
// store failure aborts the current batch, is logged as a fixed category and is
// retried on the next cycle; Run never returns while the queue is merely empty.
// Only queued jobs are ever considered.
func (s *Service) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := s.Drain(ctx); err != nil {
			logLifecycle("ai worker: batch aborted")
		}
		if ctx.Err() != nil {
			return nil
		}
		s.wait(ctx, s.interval)
	}
}

// Drain processes one bounded batch of queued jobs, one at a time, and returns
// nil when the batch is consumed or the caller's context is canceled. A store
// operation that makes further progress unsafe stops the batch and is returned;
// Run logs it and retries on the next cycle.
func (s *Service) Drain(ctx context.Context) error {
	ids, err := s.store.ListQueued(ctx)
	if err != nil {
		return fmt.Errorf("cannot list queued jobs: %w", err)
	}
	if len(ids) > batchSize {
		ids = ids[:batchSize]
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return nil
		}
		job, claimed, err := s.store.Claim(ctx, id)
		if err != nil {
			return fmt.Errorf("cannot claim job %d: %w", id, err)
		}
		if !claimed {
			continue
		}
		logLifecycle("ai job %d: claimed", job.ID)
		if err := s.process(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

// process runs one claimed job under the per-job budget and records exactly one
// terminal outcome. Timeout and cancellation take precedence over the exit
// status and over any file the process left behind.
func (s *Service) process(ctx context.Context, job Job) error {
	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	execution, runErr := s.runner.Run(runCtx, job, s.archive)
	cancel()

	if runErr != nil {
		return s.recordFailure(ctx, job.ID, failureFromRunner(runErr))
	}
	if failure, ok := classifyExecution(execution, job); ok {
		return s.recordFailure(ctx, job.ID, failure)
	}
	return s.recordSuccess(ctx, job.ID, execution.Result)
}

// recordSuccess writes a validated result on a detached persistence context.
func (s *Service) recordSuccess(ctx context.Context, id int64, result json.RawMessage) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	defer cancel()
	if err := s.store.Succeed(persistCtx, id, result); err != nil {
		return fmt.Errorf("cannot record job %d success: %w", id, err)
	}
	logLifecycle("ai job %d: succeeded", id)
	return nil
}

// recordFailure writes a failure on a detached persistence context so a
// canceled or timed-out job can still record its terminal state.
func (s *Service) recordFailure(ctx context.Context, id int64, failure Failure) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
	defer cancel()
	if err := s.store.Fail(persistCtx, id, failure); err != nil {
		return fmt.Errorf("cannot record job %d failure: %w", id, err)
	}
	logLifecycle("ai job %d: failed (%s)", id, failure.Code)
	return nil
}

// failureFromRunner maps a runner error to a fixed failure category and the
// sanitized stderr tail it carries.
func failureFromRunner(err error) Failure {
	var codeErr *CodeError
	if errors.As(err, &codeErr) {
		return Failure{Code: codeErr.Code, StderrTail: codeErr.StderrTail}
	}
	return Failure{Code: CodeProcessError}
}

// classifyExecution maps a completed process to a failure. It returns
// (Failure, true) for a failure and the zero value with false for a validated
// success. The exit status is checked before the result bytes, so a nonzero
// exit can never be accepted as a success document.
func classifyExecution(execution Execution, job Job) (Failure, bool) {
	tail := execution.StderrTail
	switch execution.ExitCode {
	case 0:
		if execution.Result == nil {
			return Failure{Code: CodeResultMissing, StderrTail: tail}, true
		}
		if err := ValidateSuccess(execution.Result, job); err != nil {
			return Failure{Code: CodeResultInvalid, StderrTail: tail}, true
		}
		return Failure{}, false
	case 2, 3:
		if execution.Result == nil {
			return Failure{Code: CodeResultMissing, StderrTail: tail}, true
		}
		code, err := ValidateFailure(execution.Result)
		if err != nil {
			return Failure{Code: CodeResultInvalid, StderrTail: tail}, true
		}
		return Failure{Code: code, StderrTail: tail}, true
	default:
		return Failure{Code: CodeProcessError, StderrTail: tail}, true
	}
}
