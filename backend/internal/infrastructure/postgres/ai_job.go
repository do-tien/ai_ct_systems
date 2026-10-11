package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/aijob"
	"nodule-reader/backend/internal/infrastructure/postgres/lc1db"
)

// AIJobStore is the LC1-12 persistence boundary for AI job lifecycle
// transitions. It only ever touches the ai_jobs table and reads the case
// linkage; it never writes reads, marks or case state.
type AIJobStore struct {
	pool *pgxpool.Pool
}

// NewAIJobStore builds the job persistence boundary on an existing pool.
func NewAIJobStore(pool *pgxpool.Pool) *AIJobStore {
	return &AIJobStore{pool: pool}
}

// ListQueued returns the IDs of every queued job in ascending order. This is
// the bounded startup snapshot; it never marks anything running.
func (s *AIJobStore) ListQueued(ctx context.Context) ([]int64, error) {
	ids, err := lc1db.New(s.pool).ListQueuedAIJobs(ctx)
	if err != nil {
		return nil, classifyJobRead(ctx, err)
	}
	return ids, nil
}

// Claim conditionally transitions one queued job to running and reads its
// immutable case linkage in the same transaction. A job that is not queued
// returns claimed=false with no error. The transaction commits before the
// caller may launch Python, so a claim is never held open across the work.
func (s *AIJobStore) Claim(ctx context.Context, id int64) (aijob.Job, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return aijob.Job{}, false, failJob(aijob.ErrDependency, "cannot open a database transaction")
	}
	defer rollbackJob(ctx, tx)

	queries := lc1db.New(s.pool).WithTx(tx)
	started, err := queries.StartAIJob(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Another claimant won, or the job is not queued: nothing to run.
			return aijob.Job{}, false, nil
		}
		return aijob.Job{}, false, classifyJobRead(ctx, err)
	}

	caseRow, err := queries.GetCase(ctx, started.CaseID)
	if err != nil {
		return aijob.Job{}, false, classifyJobRead(ctx, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return aijob.Job{}, false, commitJobFailure(ctx, err)
	}

	return aijob.Job{
		ID:                  started.ID,
		CaseID:              started.CaseID,
		OrthancSeriesID:     caseRow.OrthancSeriesID,
		SeriesInstanceUID:   caseRow.SeriesInstanceUid,
		FrameOfReferenceUID: caseRow.FrameOfReferenceUid,
	}, true, nil
}

// Succeed records a validated result on a running job. A rejected transition
// (the job is not running) is ErrConflict.
func (s *AIJobStore) Succeed(ctx context.Context, id int64, result json.RawMessage) error {
	_, err := lc1db.New(s.pool).CompleteAIJob(ctx, lc1db.CompleteAIJobParams{ID: id, Result: result})
	if err != nil {
		return classifyTerminal(ctx, err)
	}
	return nil
}

// Fail records a sanitized failure on a running job. A rejected transition
// (the job is not running) is ErrConflict.
func (s *AIJobStore) Fail(ctx context.Context, id int64, failure aijob.Failure) error {
	_, err := lc1db.New(s.pool).FailAIJob(ctx, lc1db.FailAIJobParams{
		ID:         id,
		ErrorCode:  pgtype.Text{String: failure.Code, Valid: true},
		StderrTail: pgtype.Text{String: failure.StderrTail, Valid: failure.StderrTail != ""},
	})
	if err != nil {
		return classifyTerminal(ctx, err)
	}
	return nil
}

// rollbackJob releases the connection with a context detached from the caller
// so a canceled claim still rolls back.
func rollbackJob(ctx context.Context, tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(cleanup)
}

// classifyJobRead maps a failed pre-commit statement to a category without
// leaking SQL detail. A missing linkage row is a dependency failure.
func classifyJobRead(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return failJob(aijob.ErrDependency, "job persistence canceled")
	}
	return failJob(aijob.ErrDependency, "job persistence failed")
}

// commitJobFailure keeps a definite server-side rejection as a dependency
// failure and reports a lost connection as an unknown outcome.
func commitJobFailure(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.EqualFold(pgErr.Severity, "ERROR") {
		return classifyJobRead(ctx, err)
	}
	return failJob(aijob.ErrOutcomeUnknown, "job claim commit outcome is unknown")
}

// classifyTerminal maps an autocommit terminal write failure. A rejected
// conditional update is a conflict; a definite server rejection is a
// dependency failure; a lost connection is an unknown outcome that must never
// be treated as rolled back.
func classifyTerminal(ctx context.Context, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return failJob(aijob.ErrConflict, "job is not running")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.EqualFold(pgErr.Severity, "ERROR") {
		return failJob(aijob.ErrDependency, "database rejected the job transition")
	}
	return failJob(aijob.ErrOutcomeUnknown, "job transition outcome is unknown")
}

func failJob(sentinel error, reason string) error {
	return fmt.Errorf("%w: %s", sentinel, reason)
}
