// Package initialread implements the LC1-10 independent initial read: it serves
// case metadata and persists or reloads the physician's initial-read marks.
// Coordinates are DICOM LPS millimetres and are preserved exactly; this package
// never converts, rounds or repairs geometry, and it never touches AI state.
package initialread

import (
	"context"
	"errors"
	"time"
)

// Sentinel error categories. The HTTP transport maps them to statuses; a
// category is exposed through errors.Is and never leaks adapter detail.
var (
	// ErrInvalid means the caller supplied malformed or unusable input.
	ErrInvalid = errors.New("initial read request is invalid")
	// ErrNotFound means the case does not exist.
	ErrNotFound = errors.New("case not found")
	// ErrLocked means the initial read (or its case state) no longer accepts
	// mark changes.
	ErrLocked = errors.New("initial read is locked")
	// ErrConflict means the stored initial/final read state is inconsistent with
	// the requested transition (missing draft or an unexpected final read).
	ErrConflict = errors.New("initial read submission conflicts with stored state")
	// ErrForbidden means the case's initial read belongs to another user.
	ErrForbidden = errors.New("case is assigned to another user")
	// ErrDependency means the storage dependency failed before COMMIT, so the
	// operation is known to have rolled back.
	ErrDependency = errors.New("initial read storage failed")
	// ErrCommitOutcomeUnknown means the connection was lost around COMMIT, so
	// whether the write landed is unknown.
	ErrCommitOutcomeUnknown = errors.New("initial read commit outcome is unknown")
)

// KindInitial is the reads.kind value of the independent initial read.
const KindInitial = "initial"

// KindFinal is the reads.kind value of the editable final read created by
// submission.
const KindFinal = "final"

// StatusInitialLocked is the case status after a committed submission.
const StatusInitialLocked = "initial_locked"

// OriginReader is the only mark origin permitted in the initial read. Other
// origins belong to the post-AI final read.
const OriginReader = "reader"

// failure attaches a fixed, non-identifying reason to a category sentinel. The
// reason is developer-facing: never place SQL text, credentials, UIDs or
// response bodies in it.
type failure struct {
	sentinel error
	reason   string
}

// fail builds a categorized error with a fixed reason.
func fail(sentinel error, reason string) error {
	return &failure{sentinel: sentinel, reason: reason}
}

func (e *failure) Error() string { return e.reason }

// Unwrap exposes the category sentinel to errors.Is.
func (e *failure) Unwrap() error { return e.sentinel }

// Case is the allowlisted case metadata returned by the API. It deliberately
// omits orthanc_series_id and every AI field.
type Case struct {
	ID                  int64
	Source              string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	Status              string
	CreatedAt           time.Time
	FinalizedAt         *time.Time
}

// Mark is one measurement in LPS millimetres. Vectors hold exactly three finite
// numbers; the adapter and transport never mutate or normalize them.
type Mark struct {
	P1MM            []float64
	P2MM            []float64
	CenterMM        []float64
	DiameterMM      float64
	Origin          string
	AICandidateID   *string
	ViewPlaneNormal []float64
	Note            *string
}

// Read is the initial read with its saved marks in database insertion order.
type Read struct {
	ID       int64
	CaseID   int64
	Kind     string
	Reader   string
	LockedAt *time.Time
	Marks    []Mark
}

// Store is the consumer-owned persistence port. Every method propagates the
// caller's context, receives the authenticated owner of the clinical work, and
// classifies failures into the sentinel categories above.
type Store interface {
	GetCase(ctx context.Context, caseID int64) (Case, error)
	GetOrCreateInitial(ctx context.Context, caseID, userID int64) (Read, error)
	ReplaceInitialMarks(ctx context.Context, caseID, userID int64, marks []Mark) ([]Mark, error)
	SubmitInitial(ctx context.Context, caseID, userID int64) (SubmitResult, error)
}

// SubmitResult is the committed outcome of locking the initial read and
// creating its final copy.
type SubmitResult struct {
	Status      string
	LockedAt    time.Time
	FinalReadID int64
}
