package initialread

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

// recordingStore is an in-memory Store that records what the service forwards
// to persistence, so validation and non-mutation can be asserted without a
// database.
type recordingStore struct {
	caseResult Case
	caseErr    error

	readResult Read
	readErr    error
	readCalls  int
	userSeen   []int64

	replaceResult []Mark
	replaceErr    error
	replaceCalls  int
	replaced      [][]Mark

	submitResult SubmitResult
	submitErr    error
	submitCalls  int
}

func (s *recordingStore) GetCase(context.Context, int64) (Case, error) {
	return s.caseResult, s.caseErr
}

func (s *recordingStore) GetOrCreateInitial(_ context.Context, _ int64, userID int64) (Read, error) {
	s.readCalls++
	s.userSeen = append(s.userSeen, userID)
	return s.readResult, s.readErr
}

func (s *recordingStore) ReplaceInitialMarks(_ context.Context, _ int64, userID int64, marks []Mark) ([]Mark, error) {
	s.replaceCalls++
	s.userSeen = append(s.userSeen, userID)
	s.replaced = append(s.replaced, marks)
	return s.replaceResult, s.replaceErr
}

func (s *recordingStore) SubmitInitial(_ context.Context, _ int64, userID int64) (SubmitResult, error) {
	s.submitCalls++
	s.userSeen = append(s.userSeen, userID)
	return s.submitResult, s.submitErr
}

// validMark is one ordinary reader measurement with a signed LPS geometry.
func validMark() Mark {
	return Mark{
		P1MM:            []float64{-102.3, 45.1, -180.5},
		P2MM:            []float64{-90.2, 33.0, -180.5},
		CenterMM:        []float64{-96.25, 39.05, -180.5},
		DiameterMM:      17.0,
		Origin:          "reader",
		ViewPlaneNormal: []float64{0, 0, 1},
	}
}

func TestValidateInitialMarks(t *testing.T) {
	coincident := validMark()
	coincident.P2MM = []float64{-102.3, 45.1, -180.5}

	tests := []struct {
		name     string
		marks    []Mark
		accepted bool
	}{
		{name: "empty list accepted", marks: []Mark{}, accepted: true},
		{name: "valid signed coordinates accepted", marks: []Mark{validMark()}, accepted: true},
		{
			name: "non-unit nonzero normal accepted",
			marks: []Mark{func() Mark {
				m := validMark()
				m.ViewPlaneNormal = []float64{0, 0, 2.5}
				return m
			}()}, accepted: true,
		},
		{
			name: "note preserved",
			marks: []Mark{func() Mark {
				m := validMark()
				m.Note = new("solid, 17 mm")
				return m
			}()}, accepted: true,
		},
		{
			name: "missing p1 vector rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.P1MM = nil
				return m
			}()},
		},
		{
			name: "short p2 vector rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.P2MM = []float64{-90.2, 33.0}
				return m
			}()},
		},
		{
			name: "long center vector rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.CenterMM = []float64{-96.25, 39.05, -180.5, 0}
				return m
			}()},
		},
		{
			name: "NaN coordinate rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.CenterMM = []float64{-96.25, math.NaN(), -180.5}
				return m
			}()},
		},
		{
			name: "positive infinity coordinate rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.P1MM = []float64{math.Inf(1), 45.1, -180.5}
				return m
			}()},
		},
		{
			name: "negative infinity coordinate rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.P2MM = []float64{-90.2, math.Inf(-1), -180.5}
				return m
			}()},
		},
		{name: "coincident endpoints rejected", marks: []Mark{coincident}},
		{
			name: "zero diameter rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.DiameterMM = 0
				return m
			}()},
		},
		{
			name: "negative diameter rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.DiameterMM = -1
				return m
			}()},
		},
		{
			name: "NaN diameter rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.DiameterMM = math.NaN()
				return m
			}()},
		},
		{
			name: "infinite diameter rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.DiameterMM = math.Inf(1)
				return m
			}()},
		},
		{
			name: "zero normal rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.ViewPlaneNormal = []float64{0, 0, 0}
				return m
			}()},
		},
		{
			name: "missing normal rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.ViewPlaneNormal = nil
				return m
			}()},
		},
		{
			name: "reader_after_ai origin rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.Origin = "reader_after_ai"
				return m
			}()},
		},
		{
			name: "from_ai_candidate origin rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.Origin = "from_ai_candidate"
				m.AICandidateID = new("c1")
				return m
			}()},
		},
		{
			name: "empty origin rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.Origin = ""
				return m
			}()},
		},
		{
			name: "unknown origin rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.Origin = "ai"
				return m
			}()},
		},
		{
			name: "candidate id with reader origin rejected",
			marks: []Mark{func() Mark {
				m := validMark()
				m.AICandidateID = new("c1")
				return m
			}()},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &recordingStore{}
			service := NewService(store)

			_, err := service.ReplaceMarks(context.Background(), 1, 1, test.marks)

			if !test.accepted {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("error = %v, want ErrInvalid", err)
				}
				if store.replaceCalls != 0 {
					t.Fatalf("store replaced %d times, want 0", store.replaceCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReplaceMarks error = %v, want nil", err)
			}
			if store.replaceCalls != 1 {
				t.Fatalf("store replaced %d times, want 1", store.replaceCalls)
			}
			if !reflect.DeepEqual(store.replaced[0], test.marks) {
				t.Fatalf("forwarded marks = %#v, want %#v", store.replaced[0], test.marks)
			}
		})
	}
}

func TestReplaceRejectsEntireInvalidList(t *testing.T) {
	invalid := validMark()
	invalid.DiameterMM = 0

	store := &recordingStore{}
	service := NewService(store)

	_, err := service.ReplaceMarks(context.Background(), 1, 1, []Mark{validMark(), invalid})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
	if store.replaceCalls != 0 {
		t.Fatalf("store replaced %d times, want 0", store.replaceCalls)
	}
}

func TestGetInitialForwardsUser(t *testing.T) {
	store := &recordingStore{readResult: Read{ID: 7, CaseID: 3, Kind: "initial", Reader: "Doctor A"}}
	service := NewService(store)

	read, err := service.GetInitial(context.Background(), 3, 17)
	if err != nil {
		t.Fatalf("GetInitial: %v", err)
	}
	if read.ID != 7 {
		t.Fatalf("read id = %d, want 7", read.ID)
	}
	if len(store.userSeen) != 1 || store.userSeen[0] != 17 {
		t.Fatalf("user passed to store = %v, want [17]", store.userSeen)
	}
}

func TestReplaceMarksForwardsUser(t *testing.T) {
	store := &recordingStore{}
	service := NewService(store)

	if _, err := service.ReplaceMarks(context.Background(), 3, 42, []Mark{validMark()}); err != nil {
		t.Fatalf("ReplaceMarks: %v", err)
	}
	if len(store.userSeen) != 1 || store.userSeen[0] != 42 {
		t.Fatalf("user passed to store = %v, want [42]", store.userSeen)
	}
}

func TestSubmitInitialForwardsUser(t *testing.T) {
	locked := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	store := &recordingStore{submitResult: SubmitResult{Status: StatusInitialLocked, LockedAt: locked, FinalReadID: 42}}

	result, err := NewService(store).SubmitInitial(context.Background(), 9, 23)
	if err != nil {
		t.Fatalf("SubmitInitial: %v", err)
	}
	if result.Status != StatusInitialLocked || result.FinalReadID != 42 || !result.LockedAt.Equal(locked) {
		t.Fatalf("result = %+v", result)
	}
	if len(store.userSeen) != 1 || store.userSeen[0] != 23 {
		t.Fatalf("user passed to store = %v, want [23]", store.userSeen)
	}
	if store.submitCalls != 1 {
		t.Fatalf("submit calls = %d, want 1", store.submitCalls)
	}
}

func TestInitialReadRejectsInvalidUser(t *testing.T) {
	for _, userID := range []int64{0, -1} {
		store := &recordingStore{}
		service := NewService(store)
		if _, err := service.GetInitial(context.Background(), 3, userID); !errors.Is(err, ErrInvalid) {
			t.Fatalf("GetInitial user %d error = %v, want ErrInvalid", userID, err)
		}
		if _, err := service.ReplaceMarks(context.Background(), 3, userID, []Mark{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ReplaceMarks user %d error = %v, want ErrInvalid", userID, err)
		}
		if _, err := service.SubmitInitial(context.Background(), 3, userID); !errors.Is(err, ErrInvalid) {
			t.Fatalf("SubmitInitial user %d error = %v, want ErrInvalid", userID, err)
		}
		if store.readCalls != 0 || store.replaceCalls != 0 || store.submitCalls != 0 {
			t.Fatalf("invalid user %d reached the store: %+v", userID, store.userSeen)
		}
	}
}

func TestInitialReadPropagatesForbidden(t *testing.T) {
	store := &recordingStore{readErr: fail(ErrForbidden, "assigned")}
	service := NewService(store)
	if _, err := service.GetInitial(context.Background(), 3, 17); !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
}
