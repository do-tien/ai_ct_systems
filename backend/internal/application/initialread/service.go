package initialread

import (
	"context"
	"math"
)

// Service is the LC1-10 use case: case metadata plus the independent initial
// read. It depends only on the Store port.
type Service struct {
	store Store
}

// NewService builds the use case from its persistence port.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// GetCase returns the allowlisted case metadata. It never creates a read and
// never consults AI state.
func (s *Service) GetCase(ctx context.Context, caseID int64) (Case, error) {
	return s.store.GetCase(ctx, caseID)
}

// GetInitial returns the fixed-reader initial read, creating an empty draft when
// the case is still awaiting its read and none exists.
func (s *Service) GetInitial(ctx context.Context, caseID int64) (Read, error) {
	return s.store.GetOrCreateInitial(ctx, caseID, Reader)
}

// ReplaceMarks validates the whole list and then replaces the initial read's
// marks. Nothing is persisted unless every mark is valid.
func (s *Service) ReplaceMarks(ctx context.Context, caseID int64, marks []Mark) ([]Mark, error) {
	if err := validateMarks(marks); err != nil {
		return nil, err
	}
	return s.store.ReplaceInitialMarks(ctx, caseID, Reader, marks)
}

// SubmitInitial locks the fixed-reader initial read and creates its editable
// final copy in one transaction. It never waits for or reveals AI state.
func (s *Service) SubmitInitial(ctx context.Context, caseID int64) (SubmitResult, error) {
	return s.store.SubmitInitial(ctx, caseID, Reader)
}

// validateMarks rejects the whole list on the first invalid mark. It rechecks
// the contract for every caller, including non-HTTP ones, so a short or
// non-finite vector can never reach persistence as a zero-filled triple.
func validateMarks(marks []Mark) error {
	for _, mark := range marks {
		if err := validateMark(mark); err != nil {
			return err
		}
	}
	return nil
}

func validateMark(mark Mark) error {
	if mark.Origin != OriginReader {
		return fail(ErrInvalid, "initial marks require the reader origin")
	}
	if mark.AICandidateID != nil {
		return fail(ErrInvalid, "initial marks carry no AI candidate")
	}
	if !finiteTriple(mark.ViewPlaneNormal) {
		return fail(ErrInvalid, "view normal must be three finite numbers")
	}
	if isZeroTriple(mark.ViewPlaneNormal) {
		return fail(ErrInvalid, "view normal must be nonzero")
	}
	return ValidateGeometry(mark)
}

// ValidateGeometry checks the measurement geometry shared by the independent
// initial read and the post-AI final read: P1, P2 and center are three finite
// numbers, the diameter is a positive finite number, the endpoints are distinct
// and, when supplied, the view-plane normal is three finite numbers and
// nonzero. A nil normal is accepted here because the final read may omit it;
// the initial read separately requires one. No value is ever normalized,
// rounded or repaired.
func ValidateGeometry(mark Mark) error {
	if !finiteTriple(mark.P1MM) || !finiteTriple(mark.P2MM) || !finiteTriple(mark.CenterMM) {
		return fail(ErrInvalid, "measurement vectors must be three finite numbers")
	}
	if mark.ViewPlaneNormal != nil {
		if !finiteTriple(mark.ViewPlaneNormal) {
			return fail(ErrInvalid, "view normal must be three finite numbers")
		}
		if isZeroTriple(mark.ViewPlaneNormal) {
			return fail(ErrInvalid, "view normal must be nonzero")
		}
	}
	if math.IsNaN(mark.DiameterMM) || math.IsInf(mark.DiameterMM, 0) || mark.DiameterMM <= 0 {
		return fail(ErrInvalid, "diameter must be a positive finite number")
	}
	if equalTriple(mark.P1MM, mark.P2MM) {
		return fail(ErrInvalid, "measurement endpoints must be distinct")
	}
	return nil
}

func finiteTriple(vector []float64) bool {
	if len(vector) != 3 {
		return false
	}
	for _, value := range vector {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}

func isZeroTriple(vector []float64) bool {
	return vector[0] == 0 && vector[1] == 0 && vector[2] == 0
}

func equalTriple(a, b []float64) bool {
	return a[0] == b[0] && a[1] == b[1] && a[2] == b[2]
}
