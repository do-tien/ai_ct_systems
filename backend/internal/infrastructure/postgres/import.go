package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/caseimport"
	"nodule-reader/backend/internal/infrastructure/postgres/lc1db"
)

const (
	// seriesUniqueConstraint is the auto-named UNIQUE constraint on
	// cases.series_instance_uid. Only this conflict means "already imported".
	seriesUniqueConstraint = "cases_series_instance_uid_key"
	// uniqueViolation is the SQLSTATE for a unique constraint violation.
	uniqueViolation = "23505"
	// rollbackTimeout bounds the cleanup rollback so a canceled caller cannot
	// leave the transaction open.
	rollbackTimeout = 5 * time.Second
)

// ImportStore persists one validated import as a single transaction: case,
// queued AI job and the audit event commit together or not at all.
type ImportStore struct {
	pool *pgxpool.Pool
}

// NewImportStore builds the persistence boundary on an existing pool.
func NewImportStore(pool *pgxpool.Pool) *ImportStore {
	return &ImportStore{pool: pool}
}

// CreateImportedCase inserts the case, its queued AI job and the
// case_imported event atomically and returns the new case ID. It never spans
// Orthanc requests: the caller uploads first, then calls this once.
func (s *ImportStore) CreateImportedCase(ctx context.Context, input caseimport.ImportedCase) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, caseimport.Fail(caseimport.ErrCanceled, "persistence canceled before it started")
		}
		return 0, caseimport.Fail(caseimport.ErrDependency, "cannot open a database transaction")
	}
	// The cleanup context is deliberately detached from the caller's context so
	// a canceled import still releases the connection and rolls back.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(cleanup)
	}()

	queries := lc1db.New(s.pool).WithTx(tx)

	created, err := queries.CreateCase(ctx, lc1db.CreateCaseParams{
		Source:              input.Source,
		StudyInstanceUid:    input.StudyInstanceUID,
		SeriesInstanceUid:   input.SeriesInstanceUID,
		OrthancSeriesID:     input.OrthancSeriesID,
		FrameOfReferenceUid: input.FrameOfReferenceUID,
	})
	if err != nil {
		return 0, classifyWrite(ctx, err)
	}

	if _, err := queries.CreateAIJob(ctx, created.ID); err != nil {
		return 0, classifyWrite(ctx, err)
	}

	detail, err := json.Marshal(struct {
		InstanceCount int `json:"instance_count"`
	}{InstanceCount: input.InstanceCount})
	if err != nil {
		return 0, caseimport.Fail(caseimport.ErrDependency, "cannot encode the import audit detail")
	}
	if _, err := queries.AppendEvent(ctx, lc1db.AppendEventParams{
		CaseID: pgtype.Int8{Int64: created.ID, Valid: true},
		Actor:  "importer",
		Action: "case_imported",
		Detail: detail,
	}); err != nil {
		return 0, classifyWrite(ctx, err)
	}

	if err := tx.Commit(ctx); err != nil {
		// Only a non-fatal server rejection (deferred constraint or trigger) is a
		// definite rollback and keeps its category. A fatal error, a dropped
		// connection, or a cancellation racing the COMMIT leaves the outcome
		// unknown: never claim a rollback.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && strings.EqualFold(pgErr.Severity, "ERROR") {
			return 0, classifyWrite(ctx, err)
		}
		return 0, caseimport.Fail(caseimport.ErrCommitOutcomeUnknown, "database commit outcome is unknown")
	}
	return created.ID, nil
}

// FindCaseBySeriesUID returns the case already created for a validated Series
// Instance UID. It is the commit-reconciliation lookup: it never writes and
// never reads reads, marks or AI state.
func (s *ImportStore) FindCaseBySeriesUID(ctx context.Context, seriesInstanceUID string) (int64, bool, error) {
	if seriesInstanceUID == "" {
		return 0, false, caseimport.Fail(caseimport.ErrInvalidInput, "empty series instance UID")
	}
	row, err := lc1db.New(s.pool).GetCaseBySeriesUID(ctx, seriesInstanceUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		if ctx.Err() != nil {
			return 0, false, caseimport.Fail(caseimport.ErrCanceled, "case lookup canceled")
		}
		return 0, false, caseimport.Fail(caseimport.ErrDependency, "case lookup failed")
	}
	return row.ID, true, nil
}

// classifyWrite maps a failed statement to an application category without
// leaking SQL details.
func classifyWrite(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == seriesUniqueConstraint {
		return caseimport.Fail(caseimport.ErrDuplicateSeries, "series already imported")
	}
	if ctx.Err() != nil {
		return caseimport.Fail(caseimport.ErrCanceled, "import canceled during persistence")
	}
	return caseimport.Fail(caseimport.ErrDependency, "database write failed")
}
