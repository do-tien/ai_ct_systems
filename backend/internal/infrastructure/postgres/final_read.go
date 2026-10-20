package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/finalread"
	"nodule-reader/backend/internal/application/initialread"
	"nodule-reader/backend/internal/infrastructure/postgres/lc1db"
)

// FinalReadStore persists the LC1-13 post-lock workflow: the AI projection, the
// physician-controlled final read and POC finalization. It never writes the
// independent initial read, never creates a measurement from a candidate, and
// never rewrites a finalized case.
type FinalReadStore struct {
	pool *pgxpool.Pool
}

// NewFinalReadStore builds the persistence boundary on an existing pool.
func NewFinalReadStore(pool *pgxpool.Pool) *FinalReadStore {
	return &FinalReadStore{pool: pool}
}

// GetAI projects the latest AI job of the case. Disclosure is gated on the case
// state plus a real locked initial read before any job or result byte is read: an
// awaiting case is forbidden, and a later-state case with a missing or unlocked
// initial read is a conflict, never a partial disclosure. The whole read runs in
// one repeatable-read transaction so the state, status and result agree.
func (s *FinalReadStore) GetAI(ctx context.Context, caseID, userID int64) (finalread.AI, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return finalread.AI{}, beginFinalFailure(ctx)
	}
	defer rollbackFinal(ctx, tx)

	queries := lc1db.New(s.pool).WithTx(tx)

	caseRow, err := queries.GetCase(ctx, caseID)
	if err != nil {
		return finalread.AI{}, classifyFinalRead(ctx, err)
	}
	initial, err := queries.GetRead(ctx, lc1db.GetReadParams{CaseID: caseID, Kind: initialread.KindInitial})
	if err != nil {
		return finalread.AI{}, gateInitialRead(ctx, caseRow.Status, err)
	}
	if initial.ReaderUserID != userID {
		return finalread.AI{}, failFinal(finalread.ErrAssignedToAnotherUser, "initial read belongs to another user")
	}
	if err := requireLockedInitial(caseRow.Status, initial.LockedAt, initial.ReaderUserID, userID); err != nil {
		return finalread.AI{}, err
	}

	job, err := queries.GetLatestAIJob(ctx, caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return finalread.AI{}, failFinal(finalread.ErrConflict, "case has no AI job")
	}
	if err != nil {
		return finalread.AI{}, classifyFinalRead(ctx, err)
	}

	value, err := finalread.ProjectAI(job.Status, textPointer(job.ErrorCode), job.Result, caseRow.SeriesInstanceUid, caseRow.FrameOfReferenceUid)
	if err != nil {
		return finalread.AI{}, failFinal(finalread.ErrDependency, "stored AI result is unusable")
	}
	return value, nil
}

// GetFinal returns the editable final read with its marks and decisions. It is
// strictly read-only: it never creates a missing final read and never queries
// AI. The whole read runs in one repeatable-read transaction so a concurrent
// replacement cannot produce a mixed marks/decisions snapshot.
func (s *FinalReadStore) GetFinal(ctx context.Context, caseID, userID int64) (finalread.Read, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return finalread.Read{}, beginFinalFailure(ctx)
	}
	defer rollbackFinal(ctx, tx)

	queries := lc1db.New(s.pool).WithTx(tx)

	caseRow, err := queries.GetCase(ctx, caseID)
	if err != nil {
		return finalread.Read{}, classifyFinalRead(ctx, err)
	}
	initial, err := queries.GetRead(ctx, lc1db.GetReadParams{CaseID: caseID, Kind: initialread.KindInitial})
	if err != nil {
		return finalread.Read{}, gateInitialRead(ctx, caseRow.Status, err)
	}
	if initial.ReaderUserID != userID {
		return finalread.Read{}, failFinal(finalread.ErrAssignedToAnotherUser, "initial read belongs to another user")
	}
	if err := requireLockedInitial(caseRow.Status, initial.LockedAt, initial.ReaderUserID, userID); err != nil {
		return finalread.Read{}, err
	}

	final, err := queries.GetRead(ctx, lc1db.GetReadParams{CaseID: caseID, Kind: initialread.KindFinal})
	if errors.Is(err, pgx.ErrNoRows) {
		return finalread.Read{}, failFinal(finalread.ErrConflict, "final read does not exist")
	}
	if err != nil {
		return finalread.Read{}, classifyFinalRead(ctx, err)
	}
	if final.ReaderUserID != userID {
		return finalread.Read{}, failFinal(finalread.ErrAssignedToAnotherUser, "final read belongs to another user")
	}

	marks, err := listInitialMarks(ctx, queries, final.ID)
	if err != nil {
		return finalread.Read{}, classifyFinalRead(ctx, err)
	}
	decisions, err := listDecisions(ctx, queries, final.ID)
	if err != nil {
		return finalread.Read{}, classifyFinalRead(ctx, err)
	}
	return toFinalRead(final.ID, final.CaseID, final.Kind, final.ReaderDisplayName, final.LockedAt, marks, decisions), nil
}

// ReplaceFinal atomically replaces the whole final read: its marks in request
// order and its candidate decisions ordered by candidate identifier. The case,
// the locked initial read, the editable final read and the candidate membership
// are all re-checked under a case-first lock inside this transaction, so the
// transport preflight is never the concurrency boundary.
func (s *FinalReadStore) ReplaceFinal(ctx context.Context, caseID, userID int64, value finalread.Replacement) (finalread.Read, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return finalread.Read{}, beginFinalFailure(ctx)
	}
	defer rollbackFinal(ctx, tx)

	queries := lc1db.New(s.pool).WithTx(tx)

	caseRow, err := queries.GetCaseForUpdate(ctx, caseID)
	if err != nil {
		return finalread.Read{}, classifyFinalRead(ctx, err)
	}
	if caseRow.Status == finalread.StatusAwaitingRead {
		return finalread.Read{}, failFinal(finalread.ErrForbidden, "initial read is not locked")
	}
	if caseRow.Status == finalread.StatusFinalized || caseRow.FinalizedAt.Valid {
		return finalread.Read{}, failFinal(finalread.ErrLocked, "case is finalized")
	}

	initial, err := queries.GetReadForUpdate(ctx, lc1db.GetReadForUpdateParams{CaseID: caseID, Kind: initialread.KindInitial})
	if err != nil {
		return finalread.Read{}, gateInitialRead(ctx, caseRow.Status, err)
	}
	if initial.ReaderUserID != userID {
		return finalread.Read{}, failFinal(finalread.ErrAssignedToAnotherUser, "initial read belongs to another user")
	}
	if err := requireLockedInitial(caseRow.Status, initial.LockedAt, initial.ReaderUserID, userID); err != nil {
		return finalread.Read{}, err
	}

	final, err := queries.GetReadForUpdate(ctx, lc1db.GetReadForUpdateParams{CaseID: caseID, Kind: initialread.KindFinal})
	if errors.Is(err, pgx.ErrNoRows) {
		return finalread.Read{}, failFinal(finalread.ErrConflict, "final read does not exist")
	}
	if err != nil {
		return finalread.Read{}, classifyFinalRead(ctx, err)
	}
	if final.ReaderUserID != userID {
		return finalread.Read{}, failFinal(finalread.ErrAssignedToAnotherUser, "final read belongs to another user")
	}
	if final.LockedAt.Valid {
		return finalread.Read{}, failFinal(finalread.ErrLocked, "final read is locked")
	}

	if referencesCandidates(value) {
		members, err := candidateMembership(ctx, queries, caseID, caseRow)
		if err != nil {
			return finalread.Read{}, err
		}
		if err := requireCandidateMembers(value, members); err != nil {
			return finalread.Read{}, err
		}
	}

	if err := replaceFinalMarks(ctx, queries, final.ID, value.Marks); err != nil {
		return finalread.Read{}, err
	}
	if err := replaceDecisions(ctx, queries, final.ID, value.CandidateDecisions); err != nil {
		return finalread.Read{}, err
	}

	detail, err := json.Marshal(struct {
		FinalReadID int64 `json:"final_read_id"`
	}{FinalReadID: final.ID})
	if err != nil {
		return finalread.Read{}, failFinal(finalread.ErrDependency, "cannot encode the final-read audit detail")
	}
	if _, err := queries.AppendEvent(ctx, lc1db.AppendEventParams{
		CaseID:      pgtype.Int8{Int64: caseID, Valid: true},
		Actor:       final.ReaderDisplayName,
		Action:      "final_read_saved",
		Detail:      detail,
		ActorUserID: pgtype.Int8{Int64: userID, Valid: true},
	}); err != nil {
		return finalread.Read{}, classifyFinalRead(ctx, err)
	}

	marks, err := listInitialMarks(ctx, queries, final.ID)
	if err != nil {
		return finalread.Read{}, classifyFinalRead(ctx, err)
	}
	decisions, err := listDecisions(ctx, queries, final.ID)
	if err != nil {
		return finalread.Read{}, classifyFinalRead(ctx, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return finalread.Read{}, commitFinalFailure(ctx, err)
	}
	return toFinalRead(final.ID, final.CaseID, final.Kind, final.ReaderDisplayName, final.LockedAt, marks, decisions), nil
}

// Finalize locks the final read and completes the case in one transaction. It
// never waits for AI and never decodes a result: it records only the real latest
// job status under a share lock taken after the case/read locks, so a late
// terminal job write linearizes either before or after this snapshot.
func (s *FinalReadStore) Finalize(ctx context.Context, caseID, userID int64) (finalread.FinalizeResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return finalread.FinalizeResult{}, beginFinalFailure(ctx)
	}
	defer rollbackFinal(ctx, tx)

	queries := lc1db.New(s.pool).WithTx(tx)

	caseRow, err := queries.GetCaseForUpdate(ctx, caseID)
	if err != nil {
		return finalread.FinalizeResult{}, classifyFinalRead(ctx, err)
	}
	if caseRow.Status != initialread.StatusInitialLocked || caseRow.FinalizedAt.Valid {
		return finalread.FinalizeResult{}, failFinal(finalread.ErrConflict, "case does not accept a finalize")
	}

	initial, err := queries.GetReadForUpdate(ctx, lc1db.GetReadForUpdateParams{CaseID: caseID, Kind: initialread.KindInitial})
	if err != nil {
		return finalread.FinalizeResult{}, gateInitialRead(ctx, caseRow.Status, err)
	}
	if initial.ReaderUserID != userID {
		return finalread.FinalizeResult{}, failFinal(finalread.ErrAssignedToAnotherUser, "initial read belongs to another user")
	}
	if err := requireLockedInitial(caseRow.Status, initial.LockedAt, initial.ReaderUserID, userID); err != nil {
		return finalread.FinalizeResult{}, err
	}

	final, err := queries.GetReadForUpdate(ctx, lc1db.GetReadForUpdateParams{CaseID: caseID, Kind: initialread.KindFinal})
	if errors.Is(err, pgx.ErrNoRows) {
		return finalread.FinalizeResult{}, failFinal(finalread.ErrConflict, "final read does not exist")
	}
	if err != nil {
		return finalread.FinalizeResult{}, classifyFinalRead(ctx, err)
	}
	if final.ReaderUserID != userID {
		return finalread.FinalizeResult{}, failFinal(finalread.ErrAssignedToAnotherUser, "final read belongs to another user")
	}
	if final.LockedAt.Valid {
		return finalread.FinalizeResult{}, failFinal(finalread.ErrConflict, "final read is already locked")
	}

	if _, err := queries.LockRead(ctx, final.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return finalread.FinalizeResult{}, failFinal(finalread.ErrConflict, "final read is already locked")
		}
		return finalread.FinalizeResult{}, classifyFinalRead(ctx, err)
	}

	job, err := queries.GetLatestAIJobForShare(ctx, caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return finalread.FinalizeResult{}, failFinal(finalread.ErrConflict, "case has no AI job")
	}
	if err != nil {
		return finalread.FinalizeResult{}, classifyFinalRead(ctx, err)
	}

	finalized, err := queries.FinalizeCase(ctx, caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return finalread.FinalizeResult{}, failFinal(finalread.ErrConflict, "case is no longer awaiting finalize")
	}
	if err != nil {
		return finalread.FinalizeResult{}, classifyFinalRead(ctx, err)
	}

	detail, err := json.Marshal(struct {
		FinalReadID int64  `json:"final_read_id"`
		AIStatus    string `json:"ai_status"`
	}{FinalReadID: final.ID, AIStatus: job.Status})
	if err != nil {
		return finalread.FinalizeResult{}, failFinal(finalread.ErrDependency, "cannot encode the finalize audit detail")
	}
	if _, err := queries.AppendEvent(ctx, lc1db.AppendEventParams{
		CaseID:      pgtype.Int8{Int64: caseID, Valid: true},
		Actor:       final.ReaderDisplayName,
		Action:      "case_finalized",
		Detail:      detail,
		ActorUserID: pgtype.Int8{Int64: userID, Valid: true},
	}); err != nil {
		return finalread.FinalizeResult{}, classifyFinalRead(ctx, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return finalread.FinalizeResult{}, commitFinalFailure(ctx, err)
	}
	return finalread.FinalizeResult{
		Status:      finalized.Status,
		FinalizedAt: finalized.FinalizedAt.Time,
		AIStatus:    job.Status,
	}, nil
}

// requireLockedInitial enforces the shared disclosure gate on an already-loaded
// case and initial read. The owner check is defensive: callers compare the loaded
// owner first, so a mismatch can never fall through as a disclosure.
func requireLockedInitial(caseStatus string, lockedAt pgtype.Timestamptz, initialUserID, userID int64) error {
	if caseStatus == finalread.StatusAwaitingRead {
		return failFinal(finalread.ErrForbidden, "initial read is not locked")
	}
	if !lockedAt.Valid {
		return failFinal(finalread.ErrConflict, "initial read is not locked")
	}
	if initialUserID != userID {
		return failFinal(finalread.ErrAssignedToAnotherUser, "initial read belongs to another user")
	}
	return nil
}

// gateInitialRead maps a failed initial-read load defended by the case state: an
// awaiting case is forbidden (no disclosure at all), and a later-state case
// without a readable initial read is an inconsistent conflict.
func gateInitialRead(ctx context.Context, caseStatus string, err error) error {
	if caseStatus == finalread.StatusAwaitingRead {
		return failFinal(finalread.ErrForbidden, "initial read is not locked")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return failFinal(finalread.ErrConflict, "initial read does not exist")
	}
	return classifyFinalRead(ctx, err)
}

// referencesCandidates reports whether a replacement references any AI
// candidate: a candidate-derived mark, or any candidate decision at all.
func referencesCandidates(value finalread.Replacement) bool {
	if len(value.CandidateDecisions) > 0 {
		return true
	}
	for _, mark := range value.Marks {
		if mark.Origin == finalread.OriginFromAICandidate {
			return true
		}
	}
	return false
}

// candidateMembership locks the latest job for share and returns the set of
// candidate identifiers of its validated succeeded result. A missing, nonterminal
// or failed job, or a corrupt success document, is an invalid candidate reference
// the caller maps to a 400.
func candidateMembership(ctx context.Context, queries *lc1db.Queries, caseID int64, caseRow lc1db.Case) (map[string]struct{}, error) {
	job, err := queries.GetLatestAIJobForShare(ctx, caseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, failFinal(finalread.ErrInvalid, "candidate references require an AI result")
	}
	if err != nil {
		return nil, classifyFinalRead(ctx, err)
	}
	if job.Status != finalread.AISucceeded {
		return nil, failFinal(finalread.ErrInvalid, "candidate references require a succeeded AI result")
	}
	ai, err := finalread.ProjectAI(job.Status, textPointer(job.ErrorCode), job.Result, caseRow.SeriesInstanceUid, caseRow.FrameOfReferenceUid)
	if err != nil {
		return nil, failFinal(finalread.ErrInvalid, "the AI result does not define the referenced candidates")
	}
	members := make(map[string]struct{}, len(ai.Candidates))
	for _, candidate := range ai.Candidates {
		members[candidate.ID] = struct{}{}
	}
	return members, nil
}

// requireCandidateMembers rejects any reference to a candidate that is not in the
// latest succeeded result of this case.
func requireCandidateMembers(value finalread.Replacement, members map[string]struct{}) error {
	for _, decision := range value.CandidateDecisions {
		if _, ok := members[decision.CandidateID]; !ok {
			return failFinal(finalread.ErrInvalid, "candidate decision references an unknown candidate")
		}
	}
	for _, mark := range value.Marks {
		if mark.Origin != finalread.OriginFromAICandidate {
			continue
		}
		if _, ok := members[*mark.AICandidateID]; !ok {
			return failFinal(finalread.ErrInvalid, "candidate-derived mark references an unknown candidate")
		}
	}
	return nil
}

// replaceFinalMarks deletes and re-inserts the whole mark list in request order.
func replaceFinalMarks(ctx context.Context, queries *lc1db.Queries, readID int64, marks []initialread.Mark) error {
	if err := queries.DeleteMarks(ctx, readID); err != nil {
		return classifyFinalRead(ctx, err)
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
			return classifyFinalRead(ctx, err)
		}
	}
	return nil
}

// replaceDecisions deletes and re-inserts the whole decision list. Decisions are
// stored ordered by candidate identifier so the persisted list is deterministic.
func replaceDecisions(ctx context.Context, queries *lc1db.Queries, readID int64, decisions []finalread.Decision) error {
	ordered := make([]finalread.Decision, len(decisions))
	copy(ordered, decisions)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CandidateID < ordered[j].CandidateID })

	if err := queries.DeleteCandidateDecisions(ctx, readID); err != nil {
		return classifyFinalRead(ctx, err)
	}
	for _, decision := range ordered {
		if _, err := queries.UpsertCandidateDecision(ctx, lc1db.UpsertCandidateDecisionParams{
			ReadID:        readID,
			CandidateID:   decision.CandidateID,
			Decision:      decision.Decision,
			DismissReason: textValue(decision.DismissReason),
		}); err != nil {
			return classifyFinalRead(ctx, err)
		}
	}
	return nil
}

// listDecisions loads the saved decisions in the deterministic candidate-id order.
func listDecisions(ctx context.Context, queries *lc1db.Queries, readID int64) ([]finalread.Decision, error) {
	rows, err := queries.ListCandidateDecisions(ctx, readID)
	if err != nil {
		return nil, err
	}
	decisions := make([]finalread.Decision, 0, len(rows))
	for _, row := range rows {
		decisions = append(decisions, finalread.Decision{
			CandidateID:   row.CandidateID,
			Decision:      row.Decision,
			DismissReason: textPointer(row.DismissReason),
		})
	}
	return decisions, nil
}

func toFinalRead(id, caseID int64, kind, displayName string, lockedAt pgtype.Timestamptz, marks []initialread.Mark, decisions []finalread.Decision) finalread.Read {
	return finalread.Read{
		ID:                 id,
		CaseID:             caseID,
		Kind:               kind,
		Reader:             displayName,
		LockedAt:           timePointer(lockedAt),
		Marks:              marks,
		CandidateDecisions: decisions,
	}
}

// rollbackFinal releases the connection with a context detached from the caller
// so a canceled request still rolls back.
func rollbackFinal(ctx context.Context, tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(cleanup)
}

func beginFinalFailure(ctx context.Context) error {
	return failFinal(finalread.ErrDependency, "cannot open a database transaction")
}

// commitFinalFailure keeps a definite server-side rejection as a dependency
// failure and reports a lost connection as an unknown outcome; it never claims a
// rollback it cannot prove.
func commitFinalFailure(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.EqualFold(pgErr.Severity, "ERROR") {
		return classifyFinalRead(ctx, err)
	}
	return failFinal(finalread.ErrCommitOutcomeUnknown, "final read commit outcome is unknown")
}

// classifyFinalRead maps a failed statement to an application category without
// leaking SQL detail. A missing row is ErrNotFound; every other pre-COMMIT
// failure is a dependency failure.
func classifyFinalRead(ctx context.Context, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return failFinal(finalread.ErrNotFound, "case or read not found")
	}
	if ctx.Err() != nil {
		return failFinal(finalread.ErrDependency, "final read persistence canceled")
	}
	return failFinal(finalread.ErrDependency, "final read persistence failed")
}

func failFinal(sentinel error, reason string) error {
	return fmt.Errorf("%w: %s", sentinel, reason)
}
