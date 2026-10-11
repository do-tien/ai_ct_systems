// Package finalread implements the LC1-13 post-lock workflow: the AI projection
// that may be read only after the independent initial read is locked, the
// physician-controlled final read (marks plus candidate decisions) and POC case
// finalization. It depends only on the standard library and the LC1-10/12
// application values; it never converts, rounds or repairs geometry, and it
// never creates a measurement from a candidate.
package finalread

import (
	"context"
	"errors"
	"time"

	"nodule-reader/backend/internal/application/initialread"
)

// Sentinel error categories. The HTTP transport maps them to statuses; a
// category is exposed through errors.Is and never leaks adapter detail.
var (
	// ErrInvalid means the caller supplied malformed or unusable input.
	ErrInvalid = errors.New("final read request is invalid")
	// ErrNotFound means the case does not exist.
	ErrNotFound = errors.New("case not found")
	// ErrForbidden means the case has not locked its independent initial read,
	// so no AI or final data may be disclosed.
	ErrForbidden = errors.New("initial read is not locked")
	// ErrLocked means the final read or the case is already finalized and no
	// longer accepts changes.
	ErrLocked = errors.New("final read is locked")
	// ErrConflict means the stored case/read state is inconsistent with the
	// requested transition (pre-lock finalize, missing job, repeat).
	ErrConflict = errors.New("final read conflicts with stored state")
	// ErrDependency means the storage or AI dependency failed before COMMIT, so
	// the operation is known to have rolled back.
	ErrDependency = errors.New("final read storage failed")
	// ErrCommitOutcomeUnknown means the connection was lost around COMMIT, so
	// whether the write landed is unknown.
	ErrCommitOutcomeUnknown = errors.New("final read commit outcome is unknown")
)

// Reused LC1-10 identities. The final read carries the same fixed POC reader and
// the same wire kind value the initial API already established.
const (
	// KindFinal is the reads.kind value of the editable final read.
	KindFinal = initialread.KindFinal
	// StatusAwaitingRead is the case state in which nothing may be disclosed.
	StatusAwaitingRead = "awaiting_read"
	// StatusFinalized is the case status after a committed finalize.
	StatusFinalized = "finalized"

	// OriginReader is a plain physician measurement.
	OriginReader = initialread.OriginReader
	// OriginReaderAfterAI is a physician measurement saved after seeing AI.
	OriginReaderAfterAI = "reader_after_ai"
	// OriginFromAICandidate is a physician measurement linked to one candidate.
	OriginFromAICandidate = "from_ai_candidate"

	// DecisionAccepted accepts a candidate; it carries no reason.
	DecisionAccepted = "accepted"
	// DecisionDismissed dismisses a candidate; it requires a reason.
	DecisionDismissed = "dismissed"

	// AI job statuses as projected to the browser.
	AIQueued    = "queued"
	AIRunning   = "running"
	AISucceeded = "succeeded"
	AIFailed    = "failed"

	// AIUnavailableCode is the fixed code used when a failed job carries no
	// allowlisted code.
	AIUnavailableCode = "AI_UNAVAILABLE"
	// AIUnavailableMessage is the fixed, neutral failure message. It never
	// carries stderr, exception text, paths or credentials.
	AIUnavailableMessage = "AI is unavailable; physician completion remains available"
)

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

// Decision is one physician decision about an AI candidate of the same case.
type Decision struct {
	CandidateID   string
	Decision      string
	DismissReason *string
}

// Read is the editable final read: its marks in request order and its candidate
// decisions ordered by candidate identifier.
type Read struct {
	ID                 int64
	CaseID             int64
	Kind               string
	Reader             string
	LockedAt           *time.Time
	Marks              []initialread.Mark
	CandidateDecisions []Decision
}

// Replacement replaces the whole final read in one atomic write. Both lists
// must be supplied; an omitted list is rejected, an empty list is a valid clear.
type Replacement struct {
	Marks              []initialread.Mark
	CandidateDecisions []Decision
}

// FinalizeResult is the committed outcome of completing the case.
type FinalizeResult struct {
	Status      string
	FinalizedAt time.Time
	AIStatus    string
}

// Candidate is one AI candidate projected to the browser. It carries the
// detector center/size in LPS millimetres and the raw score; it is not a
// clinical measurement and no endpoints are derived from it.
type Candidate struct {
	ID       string
	CenterMM []float64
	SizeMM   []float64
	Score    float64
}

// AIModel is the allowlisted detector provenance of a succeeded result.
type AIModel struct {
	Bundle         string
	BundleVersion  string
	ScoreThreshold float64
}

// AIInput is the allowlisted input geometry of a succeeded result.
type AIInput struct {
	NumSlices  int
	SpacingMM  []float64
	SizeVoxels []int
}

// AITiming is the allowlisted timing of a succeeded result, in seconds.
type AITiming struct {
	Load      float64
	Inference float64
	Total     float64
}

// AIResources is the allowlisted device of a succeeded result.
type AIResources struct {
	Device string
}

// AIError is the fixed, sanitized failure projection.
type AIError struct {
	Code    string
	Message string
}

// AI is the state-specific projection of the latest AI job of one case. Only
// the status is always meaningful: a queued/running job carries no candidates, a
// failed job carries an empty candidate list and a fixed error, and a succeeded
// job carries the validated candidates, model, input, timing and resources.
type AI struct {
	Status     string
	Candidates []Candidate
	Error      *AIError
	Model      *AIModel
	Input      *AIInput
	Timing     *AITiming
	Resources  *AIResources
}

// Store is the consumer-owned persistence port. Every method propagates the
// caller's context and classifies failures into the sentinel categories above.
type Store interface {
	GetAI(ctx context.Context, caseID int64, reader string) (AI, error)
	GetFinal(ctx context.Context, caseID int64, reader string) (Read, error)
	ReplaceFinal(ctx context.Context, caseID int64, reader string, value Replacement) (Read, error)
	Finalize(ctx context.Context, caseID int64, reader string) (FinalizeResult, error)
}
