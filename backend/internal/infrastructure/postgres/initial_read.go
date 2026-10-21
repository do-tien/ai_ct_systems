package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/initialread"
	"nodule-reader/backend/internal/infrastructure/postgres/lc1db"
)

// statusAwaitingRead is the only case state in which the initial read may be
// created or changed.
const statusAwaitingRead = "awaiting_read"

// InitialReadStore persists case metadata and the independent initial read on an
// existing pool. It never reads or writes AI tables.
type InitialReadStore struct {
	pool *pgxpool.Pool
}

// NewInitialReadStore builds the persistence boundary on an existing pool.
func NewInitialReadStore(pool *pgxpool.Pool) *InitialReadStore {
	return &InitialReadStore{pool: pool}
}

// GetCase returns the allowlisted case projection. It never creates a read.
func (s *InitialReadStore) GetCase(ctx context.Context, caseID int64) (initialread.Case, error) {
	row, err := lc1db.New(s.pool).GetCase(ctx, caseID)
	if err != nil {
		return initialread.Case{}, classifyInitialRead(ctx, err)
	}
	return toCase(row), nil
}

// ListCases returns the authenticated reader's cases, newest first. Ownership
// follows the initial read; a confirmed but unopened case belongs to the user
// who imported it. The projection is the same allowlist as GetCase.
func (s *InitialReadStore) ListCases(ctx context.Context, userID int64, limit int) ([]initialread.Case, error) {
	rows, err := lc1db.New(s.pool).ListCasesByReader(ctx, lc1db.ListCasesByReaderParams{
		UserID:   userID,
		RowLimit: int32(limit),
	})
	if err != nil {
		return nil, classifyInitialRead(ctx, err)
	}
	cases := make([]initialread.Case, 0, len(rows))
	for _, row := range rows {
		cases = append(cases, toCase(row))
	}
	return cases, nil
}

// GetOrCreateInitial returns the authenticated owner's initial read, creating an
// empty draft only when the case is still awaiting its read. A later-state case
// without a read is a conflict, never a fabricated draft, and a read owned by
// another user is forbidden. The case row is locked before the read.
func (s *InitialReadStore) GetOrCreateInitial(ctx context.Context, caseID, userID int64) (initialread.Read, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return initialread.Read{}, beginFailure(ctx)
	}
	defer rollbackInitial(ctx, tx)

	queries := lc1db.New(s.pool).WithTx(tx)

	caseRow, err := queries.GetCaseForUpdate(ctx, caseID)
	if err != nil {
		return initialread.Read{}, classifyInitialRead(ctx, err)
	}

	existing, err := queries.GetReadForUpdate(ctx, lc1db.GetReadForUpdateParams{CaseID: caseID, Kind: initialread.KindInitial})
	var readID, readCaseID int64
	var readKind, readReader string
	var readLockedAt pgtype.Timestamptz
	switch {
	case err == nil:
		if existing.ReaderUserID != userID {
			return initialread.Read{}, failInitial(initialread.ErrForbidden, "initial read belongs to another user")
		}
		// Existing read (locked or not) is returned unchanged.
		readID, readCaseID, readKind, readReader, readLockedAt = existing.ID, existing.CaseID, existing.Kind, existing.ReaderDisplayName, existing.LockedAt
	case errors.Is(err, pgx.ErrNoRows):
		if caseRow.Status != statusAwaitingRead {
			return initialread.Read{}, failInitial(initialread.ErrLocked, "case no longer accepts an initial read")
		}
		created, createErr := queries.CreateRead(ctx, lc1db.CreateReadParams{
			CaseID:       caseID,
			Kind:         initialread.KindInitial,
			ReaderUserID: userID,
		})
		if createErr != nil {
			return initialread.Read{}, classifyInitialRead(ctx, createErr)
		}
		readID, readCaseID, readKind, readReader, readLockedAt = created.ID, created.CaseID, created.Kind, created.ReaderDisplayName, created.LockedAt
	default:
		return initialread.Read{}, classifyInitialRead(ctx, err)
	}

	marks, err := listInitialMarks(ctx, queries, readID)
	if err != nil {
		return initialread.Read{}, classifyInitialRead(ctx, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return initialread.Read{}, commitFailure(ctx, err)
	}
	return toRead(readID, readCaseID, readKind, readReader, readLockedAt, marks), nil
}

// ReplaceInitialMarks atomically replaces every mark of the initial read. The
// case state and lock are checked under the same case-row lock, and the old
// list is deleted and the new one inserted in request order inside one
// transaction: a failure before COMMIT leaves the original list intact.
func (s *InitialReadStore) ReplaceInitialMarks(ctx context.Context, caseID, userID int64, marks []initialread.Mark) ([]initialread.Mark, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, beginFailure(ctx)
	}
	defer rollbackInitial(ctx, tx)

	queries := lc1db.New(s.pool).WithTx(tx)

	caseRow, err := queries.GetCaseForUpdate(ctx, caseID)
	if err != nil {
		return nil, classifyInitialRead(ctx, err)
	}

	read, err := queries.GetReadForUpdate(ctx, lc1db.GetReadForUpdateParams{CaseID: caseID, Kind: initialread.KindInitial})
	var readID int64
	switch {
	case err == nil:
		if read.ReaderUserID != userID {
			return nil, failInitial(initialread.ErrForbidden, "initial read belongs to another user")
		}
		if caseRow.Status != statusAwaitingRead || read.LockedAt.Valid {
			return nil, failInitial(initialread.ErrLocked, "initial read is locked")
		}
		readID = read.ID
	case errors.Is(err, pgx.ErrNoRows):
		if caseRow.Status != statusAwaitingRead {
			return nil, failInitial(initialread.ErrLocked, "case no longer accepts an initial read")
		}
		created, createErr := queries.CreateRead(ctx, lc1db.CreateReadParams{
			CaseID:       caseID,
			Kind:         initialread.KindInitial,
			ReaderUserID: userID,
		})
		if createErr != nil {
			return nil, classifyInitialRead(ctx, createErr)
		}
		readID = created.ID
	default:
		return nil, classifyInitialRead(ctx, err)
	}

	if err := queries.DeleteMarks(ctx, readID); err != nil {
		return nil, classifyInitialRead(ctx, err)
	}
	for _, mark := range marks {
		if _, err := queries.CreateMark(ctx, lc1db.CreateMarkParams{
			ReadID:          readID,
			P1Mm:            mark.P1MM,
			P2Mm:            mark.P2MM,
			CenterMm:        mark.CenterMM,
			DiameterMm:      mark.DiameterMM,
			Origin:          mark.Origin,
			AiCandidateID:   textValue(mark.AICandidateID),
			ViewPlaneNormal: mark.ViewPlaneNormal,
			Note:            textValue(mark.Note),
		}); err != nil {
			return nil, classifyInitialRead(ctx, err)
		}
	}

	saved, err := listInitialMarks(ctx, queries, readID)
	if err != nil {
		return nil, classifyInitialRead(ctx, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, commitFailure(ctx, err)
	}
	return saved, nil
}

// SubmitInitial locks the authenticated owner's initial read, creates its
// editable final read and copies every stored mark, then transitions the case
// and appends the audit event. All of it commits together or not at all.
func (s *InitialReadStore) SubmitInitial(ctx context.Context, caseID, userID int64) (initialread.SubmitResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return initialread.SubmitResult{}, beginFailure(ctx)
	}
	defer rollbackInitial(ctx, tx)

	queries := lc1db.New(s.pool).WithTx(tx)

	caseRow, err := queries.GetCaseForUpdate(ctx, caseID)
	if err != nil {
		return initialread.SubmitResult{}, classifyInitialRead(ctx, err)
	}

	initial, err := queries.GetReadForUpdate(ctx, lc1db.GetReadForUpdateParams{CaseID: caseID, Kind: initialread.KindInitial})
	if errors.Is(err, pgx.ErrNoRows) {
		return initialread.SubmitResult{}, failInitial(initialread.ErrConflict, "initial read does not exist")
	}
	if err != nil {
		return initialread.SubmitResult{}, classifyInitialRead(ctx, err)
	}
	if initial.ReaderUserID != userID {
		return initialread.SubmitResult{}, failInitial(initialread.ErrForbidden, "initial read belongs to another user")
	}
	if caseRow.Status != statusAwaitingRead || caseRow.FinalizedAt.Valid {
		return initialread.SubmitResult{}, failInitial(initialread.ErrLocked, "case does not accept an initial submit")
	}
	if initial.LockedAt.Valid {
		return initialread.SubmitResult{}, failInitial(initialread.ErrLocked, "initial read is already locked")
	}

	if _, err := queries.GetReadForUpdate(ctx, lc1db.GetReadForUpdateParams{CaseID: caseID, Kind: initialread.KindFinal}); err == nil {
		return initialread.SubmitResult{}, failInitial(initialread.ErrConflict, "final read already exists")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return initialread.SubmitResult{}, classifyInitialRead(ctx, err)
	}

	locked, err := queries.LockRead(ctx, initial.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent submit locked the row between the check and the update.
		return initialread.SubmitResult{}, failInitial(initialread.ErrLocked, "initial read is already locked")
	}
	if err != nil {
		return initialread.SubmitResult{}, classifyInitialRead(ctx, err)
	}

	final, err := queries.CreateRead(ctx, lc1db.CreateReadParams{
		CaseID:       caseID,
		Kind:         initialread.KindFinal,
		ReaderUserID: userID,
	})
	if err != nil {
		return initialread.SubmitResult{}, classifyInitialRead(ctx, err)
	}

	if err := queries.CopyMarks(ctx, lc1db.CopyMarksParams{SourceReadID: initial.ID, TargetReadID: final.ID}); err != nil {
		return initialread.SubmitResult{}, classifyInitialRead(ctx, err)
	}

	if _, err := queries.UpdateCaseState(ctx, lc1db.UpdateCaseStateParams{
		ID:     caseID,
		Status: initialread.StatusInitialLocked,
	}); err != nil {
		return initialread.SubmitResult{}, classifyInitialRead(ctx, err)
	}

	detail, err := json.Marshal(struct {
		InitialReadID int64 `json:"initial_read_id"`
		FinalReadID   int64 `json:"final_read_id"`
	}{InitialReadID: initial.ID, FinalReadID: final.ID})
	if err != nil {
		return initialread.SubmitResult{}, failInitial(initialread.ErrDependency, "cannot encode the submit audit detail")
	}
	if _, err := queries.AppendEvent(ctx, lc1db.AppendEventParams{
		CaseID:      pgtype.Int8{Int64: caseID, Valid: true},
		Actor:       initial.ReaderDisplayName,
		Action:      "initial_submitted",
		Detail:      detail,
		ActorUserID: pgtype.Int8{Int64: userID, Valid: true},
	}); err != nil {
		return initialread.SubmitResult{}, classifyInitialRead(ctx, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return initialread.SubmitResult{}, commitFailure(ctx, err)
	}
	return initialread.SubmitResult{
		Status:      initialread.StatusInitialLocked,
		LockedAt:    locked.LockedAt.Time,
		FinalReadID: final.ID,
	}, nil
}

func listInitialMarks(ctx context.Context, queries *lc1db.Queries, readID int64) ([]initialread.Mark, error) {
	rows, err := queries.ListMarks(ctx, readID)
	if err != nil {
		return nil, err
	}
	marks := make([]initialread.Mark, 0, len(rows))
	for _, row := range rows {
		marks = append(marks, toMark(row))
	}
	return marks, nil
}

// rollbackInitial releases the connection with a context detached from the
// caller so a canceled request still rolls back.
func rollbackInitial(ctx context.Context, tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(cleanup)
}

func beginFailure(ctx context.Context) error {
	return failInitial(initialread.ErrDependency, "cannot open a database transaction")
}

// commitFailure keeps a definite server-side rejection as a dependency failure
// and reports a lost connection as an unknown outcome; it never claims a
// rollback it cannot prove.
func commitFailure(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.EqualFold(pgErr.Severity, "ERROR") {
		return classifyInitialRead(ctx, err)
	}
	return failInitial(initialread.ErrCommitOutcomeUnknown, "initial read commit outcome is unknown")
}

// classifyInitialRead maps a failed statement to an application category
// without leaking SQL detail. A missing row is ErrNotFound; every other
// pre-COMMIT failure is a dependency failure.
func classifyInitialRead(ctx context.Context, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return failInitial(initialread.ErrNotFound, "case or read not found")
	}
	return failInitial(initialread.ErrDependency, "initial read storage failed")
}

func failInitial(sentinel error, reason string) error {
	return fmt.Errorf("%w: %s", sentinel, reason)
}

// textValue converts a nullable application string to its database form.
func textValue(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *value, Valid: true}
}

// textPointer converts a nullable database string to its application form.
func textPointer(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	text := value.String
	return &text
}

func timePointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	instant := value.Time
	return &instant
}

func toCase(row lc1db.Case) initialread.Case {
	return initialread.Case{
		ID:                  row.ID,
		Source:              row.Source,
		StudyInstanceUID:    row.StudyInstanceUid,
		SeriesInstanceUID:   row.SeriesInstanceUid,
		FrameOfReferenceUID: row.FrameOfReferenceUid,
		Status:              row.Status,
		CreatedAt:           row.CreatedAt.Time,
		FinalizedAt:         timePointer(row.FinalizedAt),
	}
}

func toRead(id, caseID int64, kind, displayName string, lockedAt pgtype.Timestamptz, marks []initialread.Mark) initialread.Read {
	return initialread.Read{
		ID:       id,
		CaseID:   caseID,
		Kind:     kind,
		Reader:   displayName,
		LockedAt: timePointer(lockedAt),
		Marks:    marks,
	}
}

func toMark(row lc1db.Mark) initialread.Mark {
	return initialread.Mark{
		P1MM:            row.P1Mm,
		P2MM:            row.P2Mm,
		CenterMM:        row.CenterMm,
		DiameterMM:      row.DiameterMm,
		Origin:          row.Origin,
		AICandidateID:   textPointer(row.AiCandidateID),
		ViewPlaneNormal: row.ViewPlaneNormal,
		Note:            textPointer(row.Note),
	}
}
