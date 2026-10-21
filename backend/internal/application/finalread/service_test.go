package finalread

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"nodule-reader/backend/internal/application/initialread"
)

// recordingStore is a stateful stub that keeps the last replacement it accepted
// so a test can prove a rejected validation never reached persistence.
type recordingStore struct {
	saved     Replacement
	saves     int
	reads     int
	aiReads   int
	finalizes int
	userSeen  []int64
	aiErr     error
}

func (s *recordingStore) GetAI(_ context.Context, _ int64, userID int64) (AI, error) {
	s.aiReads++
	s.userSeen = append(s.userSeen, userID)
	if s.aiErr != nil {
		return AI{}, s.aiErr
	}
	return AI{Status: AIQueued}, nil
}

func (s *recordingStore) GetFinal(_ context.Context, _ int64, userID int64) (Read, error) {
	s.reads++
	s.userSeen = append(s.userSeen, userID)
	return Read{}, nil
}

func (s *recordingStore) ReplaceFinal(_ context.Context, caseID int64, userID int64, value Replacement) (Read, error) {
	s.saves++
	s.userSeen = append(s.userSeen, userID)
	s.saved = value
	return Read{ID: 1, CaseID: caseID, Kind: KindFinal, Marks: value.Marks, CandidateDecisions: value.CandidateDecisions}, nil
}

func (s *recordingStore) Finalize(_ context.Context, _ int64, userID int64) (FinalizeResult, error) {
	s.finalizes++
	s.userSeen = append(s.userSeen, userID)
	return FinalizeResult{}, nil
}

func geometry(p1, p2, center []float64, diameter float64) initialread.Mark {
	return initialread.Mark{
		P1MM:            p1,
		P2MM:            p2,
		CenterMM:        center,
		DiameterMM:      diameter,
		Origin:          OriginReader,
		ViewPlaneNormal: []float64{0, 0, 1},
	}
}

func readerMark(note *string) initialread.Mark {
	mark := geometry(
		[]float64{-102.3, 45.1, -180.5},
		[]float64{-90.2, 33.0, -180.5},
		[]float64{-96.25, 39.05, -180.5},
		17.0)
	mark.Note = note
	return mark
}

func withOrigin(mark initialread.Mark, origin string, candidateID *string) initialread.Mark {
	mark.Origin = origin
	mark.AICandidateID = candidateID
	return mark
}

func TestFinalReadValidation(t *testing.T) {
	reason := "vessel, not a nodule"

	tests := []struct {
		name    string
		value   Replacement
		wantErr bool
	}{
		{name: "absent marks list rejected", value: Replacement{CandidateDecisions: []Decision{}}, wantErr: true},
		{name: "absent decisions list rejected", value: Replacement{Marks: []initialread.Mark{}}, wantErr: true},
		{name: "empty replacement succeeds", value: Replacement{Marks: []initialread.Mark{}, CandidateDecisions: []Decision{}}},
		{
			name: "accepted with null reason succeeds",
			value: Replacement{
				Marks:              []initialread.Mark{withOrigin(readerMark(nil), OriginFromAICandidate, new("c1"))},
				CandidateDecisions: []Decision{{CandidateID: "c1", Decision: DecisionAccepted}},
			},
		},
		{
			name: "dismissed with reason succeeds",
			value: Replacement{
				Marks:              []initialread.Mark{readerMark(nil)},
				CandidateDecisions: []Decision{{CandidateID: "c1", Decision: DecisionDismissed, DismissReason: &reason}},
			},
		},
		{
			name: "dismissed with whitespace reason fails",
			value: Replacement{
				Marks:              []initialread.Mark{},
				CandidateDecisions: []Decision{{CandidateID: "c1", Decision: DecisionDismissed, DismissReason: new(" \t ")}},
			},
			wantErr: true,
		},
		{
			name: "accepted with reason fails",
			value: Replacement{
				Marks:              []initialread.Mark{},
				CandidateDecisions: []Decision{{CandidateID: "c1", Decision: DecisionAccepted, DismissReason: &reason}},
			},
			wantErr: true,
		},
		{
			name: "unknown decision fails",
			value: Replacement{
				Marks:              []initialread.Mark{},
				CandidateDecisions: []Decision{{CandidateID: "c1", Decision: "maybe"}},
			},
			wantErr: true,
		},
		{
			name: "duplicate decision fails",
			value: Replacement{
				Marks: []initialread.Mark{},
				CandidateDecisions: []Decision{
					{CandidateID: "c1", Decision: DecisionAccepted},
					{CandidateID: "c1", Decision: DecisionAccepted},
				},
			},
			wantErr: true,
		},
		{
			name: "blank decision candidate id fails",
			value: Replacement{
				Marks:              []initialread.Mark{},
				CandidateDecisions: []Decision{{CandidateID: "  ", Decision: DecisionAccepted}},
			},
			wantErr: true,
		},
		{
			name: "reader origin with candidate id fails",
			value: Replacement{
				Marks:              []initialread.Mark{withOrigin(readerMark(nil), OriginReader, new("c1"))},
				CandidateDecisions: []Decision{},
			},
			wantErr: true,
		},
		{
			name: "candidate origin without candidate id fails",
			value: Replacement{
				Marks:              []initialread.Mark{withOrigin(readerMark(nil), OriginFromAICandidate, nil)},
				CandidateDecisions: []Decision{},
			},
			wantErr: true,
		},
		{
			name: "unknown origin fails",
			value: Replacement{
				Marks:              []initialread.Mark{withOrigin(readerMark(nil), "ai", nil)},
				CandidateDecisions: []Decision{},
			},
			wantErr: true,
		},
		{
			name: "reader_after_ai origin succeeds",
			value: Replacement{
				Marks:              []initialread.Mark{withOrigin(readerMark(nil), OriginReaderAfterAI, nil)},
				CandidateDecisions: []Decision{},
			},
		},
		{
			name: "nil optional normal succeeds",
			value: Replacement{
				Marks: []initialread.Mark{func() initialread.Mark {
					mark := readerMark(nil)
					mark.ViewPlaneNormal = nil
					return mark
				}()},
				CandidateDecisions: []Decision{},
			},
		},
		{
			name: "zero normal fails",
			value: Replacement{
				Marks: []initialread.Mark{func() initialread.Mark {
					mark := readerMark(nil)
					mark.ViewPlaneNormal = []float64{0, 0, 0}
					return mark
				}()},
				CandidateDecisions: []Decision{},
			},
			wantErr: true,
		},
		{
			name: "short normal fails",
			value: Replacement{
				Marks: []initialread.Mark{func() initialread.Mark {
					mark := readerMark(nil)
					mark.ViewPlaneNormal = []float64{0, 1}
					return mark
				}()},
				CandidateDecisions: []Decision{},
			},
			wantErr: true,
		},
		{
			name: "nonfinite normal fails",
			value: Replacement{
				Marks: []initialread.Mark{func() initialread.Mark {
					mark := readerMark(nil)
					mark.ViewPlaneNormal = []float64{0, 1, math.Inf(1)}
					return mark
				}()},
				CandidateDecisions: []Decision{},
			},
			wantErr: true,
		},
		{
			name: "candidate mark without accepted decision fails",
			value: Replacement{
				Marks:              []initialread.Mark{withOrigin(readerMark(nil), OriginFromAICandidate, new("c1"))},
				CandidateDecisions: []Decision{{CandidateID: "c1", Decision: DecisionDismissed, DismissReason: &reason}},
			},
			wantErr: true,
		},
		{
			name: "candidate mark with absent decision fails",
			value: Replacement{
				Marks:              []initialread.Mark{withOrigin(readerMark(nil), OriginFromAICandidate, new("c1"))},
				CandidateDecisions: []Decision{},
			},
			wantErr: true,
		},
		{
			name: "two marks linked to one candidate fail",
			value: Replacement{
				Marks: []initialread.Mark{
					withOrigin(readerMark(nil), OriginFromAICandidate, new("c1")),
					withOrigin(readerMark(new("second")), OriginFromAICandidate, new("c1")),
				},
				CandidateDecisions: []Decision{{CandidateID: "c1", Decision: DecisionAccepted}},
			},
			wantErr: true,
		},
		{
			name: "accepted decision without a mark succeeds",
			value: Replacement{
				Marks:              []initialread.Mark{},
				CandidateDecisions: []Decision{{CandidateID: "c1", Decision: DecisionAccepted}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &recordingStore{}
			service := NewService(store)
			_, err := service.ReplaceFinal(context.Background(), 1, 1, test.value)
			if test.wantErr {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("error = %v, want ErrInvalid", err)
				}
				if store.saves != 0 {
					t.Fatalf("store saved %d times, want 0", store.saves)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			if store.saves != 1 {
				t.Fatalf("store saved %d times, want 1", store.saves)
			}
		})
	}
}

// TestFinalReadPreservesSubmittedValues proves negative LPS coordinates and the
// submitted dismiss reason survive validation byte-for-byte, and that a rejected
// whole-list replacement leaves a stateful store's snapshot untouched.
func TestFinalReadPreservesSubmittedValues(t *testing.T) {
	reason := "mạch máu, không phải nốt"
	store := &recordingStore{}
	service := NewService(store)

	mark := geometry(
		[]float64{-102.3, 45.1, -180.5},
		[]float64{-90.2, 33.0, -180.5},
		[]float64{-96.25, 39.05, -180.5},
		17.0)
	value := Replacement{
		Marks:              []initialread.Mark{mark, withOrigin(readerMark(nil), OriginFromAICandidate, new("c1"))},
		CandidateDecisions: []Decision{{CandidateID: "c1", Decision: DecisionDismissed, DismissReason: &reason}},
	}

	// The candidate-derived mark needs an accepted decision, so this replacement
	// is invalid: nothing may be saved.
	if _, err := service.ReplaceFinal(context.Background(), 1, 1, value); !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want ErrInvalid", err)
	}
	if store.saves != 0 {
		t.Fatalf("store saved %d times after a rejected replacement, want 0", store.saves)
	}

	valid := Replacement{
		Marks:              []initialread.Mark{mark},
		CandidateDecisions: []Decision{{CandidateID: "c1", Decision: DecisionDismissed, DismissReason: &reason}},
	}
	if _, err := service.ReplaceFinal(context.Background(), 1, 1, valid); err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if !reflect.DeepEqual(store.saved.Marks[0].P1MM, []float64{-102.3, 45.1, -180.5}) ||
		!reflect.DeepEqual(store.saved.Marks[0].CenterMM, []float64{-96.25, 39.05, -180.5}) {
		t.Fatalf("coordinates changed: %+v", store.saved.Marks[0])
	}
	if store.saved.CandidateDecisions[0].DismissReason == nil || *store.saved.CandidateDecisions[0].DismissReason != reason {
		t.Fatalf("reason changed: %+v", store.saved.CandidateDecisions[0])
	}
}

func TestProjectAI(t *testing.T) {
	const series = "1.2.840.113619.2.55.3.604688.100.1"
	const frame = "1.2.840.113619.2.55.3.604688.100.3"

	t.Run("queued and running carry no candidates", func(t *testing.T) {
		for _, status := range []string{AIQueued, AIRunning} {
			value, err := ProjectAI(status, nil, nil, series, frame)
			if err != nil {
				t.Fatalf("%s: error = %v", status, err)
			}
			if value.Status != status || value.Candidates != nil || value.Error != nil ||
				value.Model != nil || value.Input != nil || value.Timing != nil || value.Resources != nil {
				t.Fatalf("%s projection = %+v", status, value)
			}
		}
	})

	t.Run("failed projects the fixed message and allowlisted code", func(t *testing.T) {
		code := "AI_TIMEOUT"
		value, err := ProjectAI(AIFailed, &code, nil, series, frame)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if value.Status != AIFailed || value.Error == nil || value.Error.Code != "AI_TIMEOUT" ||
			value.Error.Message != AIUnavailableMessage {
			t.Fatalf("projection = %+v", value)
		}
		if value.Candidates == nil || len(value.Candidates) != 0 {
			t.Fatalf("candidates = %#v, want empty non-nil", value.Candidates)
		}

		unknown := "SEGRETO_ERR"
		value, err = ProjectAI(AIFailed, &unknown, nil, series, frame)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if value.Error.Code != AIUnavailableCode {
			t.Fatalf("unknown code projected as %q, want %q", value.Error.Code, AIUnavailableCode)
		}

		value, err = ProjectAI(AIFailed, nil, nil, series, frame)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if value.Error.Code != AIUnavailableCode {
			t.Fatalf("null code projected as %q, want %q", value.Error.Code, AIUnavailableCode)
		}
	})

	t.Run("succeeded projects only allowlisted members", func(t *testing.T) {
		document := map[string]any{
			"schema_version":         "1",
			"series_instance_uid":    series,
			"frame_of_reference_uid": frame,
			"coordinate_system":      "DICOM_LPS_mm",
			"model": map[string]any{
				"bundle":            "lung_nodule_ct_detection",
				"bundle_version":    "0.6.11",
				"monai_version":     "1.4.0",
				"torch_version":     "2.7.0",
				"affine_lps_to_ras": true,
				"score_threshold":   0.5,
			},
			"input": map[string]any{
				"num_slices":  133,
				"spacing_mm":  []any{0.7, 0.7, 1.0},
				"size_voxels": []any{512, 512, 133},
			},
			"candidates": []any{
				map[string]any{
					"id":        "c1",
					"center_mm": []any{-96.25, 39.05, -180.5},
					"size_mm":   []any{11.2, 9.4, 8.0},
					"score":     0.94,
					"ignored":   "dropped",
				},
			},
			"timing_s":  map[string]any{"load": 1.5, "inference": 2.5, "total": 4.0},
			"resources": map[string]any{"device": "cuda", "peak_gpu_mem_mb": 1, "peak_rss_mb": 2},
		}
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		value, err := ProjectAI(AISucceeded, nil, raw, series, frame)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if value.Status != AISucceeded || value.Error != nil {
			t.Fatalf("status/error = %+v", value)
		}
		if len(value.Candidates) != 1 || value.Candidates[0].ID != "c1" || value.Candidates[0].Score != 0.94 {
			t.Fatalf("candidates = %+v", value.Candidates)
		}
		if value.Model == nil || value.Model.Bundle != "lung_nodule_ct_detection" || value.Model.ScoreThreshold != 0.5 {
			t.Fatalf("model = %+v", value.Model)
		}
		if value.Input == nil || value.Input.NumSlices != 133 {
			t.Fatalf("input = %+v", value.Input)
		}
		if value.Timing == nil || value.Timing.Total != 4.0 {
			t.Fatalf("timing = %+v", value.Timing)
		}
		if value.Resources == nil || value.Resources.Device != "cuda" {
			t.Fatalf("resources = %+v", value.Resources)
		}
	})

	t.Run("succeeded with empty candidates stays a valid success", func(t *testing.T) {
		document := map[string]any{
			"schema_version":         "1",
			"series_instance_uid":    series,
			"frame_of_reference_uid": frame,
			"coordinate_system":      "DICOM_LPS_mm",
			"model": map[string]any{
				"bundle":            "lung_nodule_ct_detection",
				"bundle_version":    "0.6.11",
				"monai_version":     "1.4.0",
				"torch_version":     "2.7.0",
				"affine_lps_to_ras": true,
				"score_threshold":   0.5,
			},
			"input":      map[string]any{"num_slices": 1, "spacing_mm": []any{1.0, 1.0, 1.0}, "size_voxels": []any{1, 1, 1}},
			"candidates": []any{},
			"timing_s":   map[string]any{"load": 0.0, "inference": 0.0, "total": 0.0},
			"resources":  map[string]any{"device": "cpu", "peak_gpu_mem_mb": 0, "peak_rss_mb": 0},
		}
		raw, _ := json.Marshal(document)
		value, err := ProjectAI(AISucceeded, nil, raw, series, frame)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if value.Candidates == nil || len(value.Candidates) != 0 {
			t.Fatalf("candidates = %#v, want empty non-nil", value.Candidates)
		}
	})

	t.Run("corrupt success is rejected", func(t *testing.T) {
		cases := map[string][]byte{
			"wrong uid":     mustJSON(t, map[string]any{"schema_version": "1"}),
			"not an object": []byte(`[1,2,3]`),
			"duplicate key": []byte(`{"schema_version":"1","schema_version":"1"}`),
		}
		for name, raw := range cases {
			if _, err := ProjectAI(AISucceeded, nil, raw, series, frame); err == nil {
				t.Fatalf("%s: error = nil, want rejection", name)
			}
		}
	})

	t.Run("unknown status is rejected", func(t *testing.T) {
		if _, err := ProjectAI("weird", nil, nil, series, frame); err == nil {
			t.Fatalf("error = nil, want rejection")
		}
	})
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// TestServiceForwardsUserID proves every use case passes the authenticated user
// through and keeps reads separate from writes.
func TestServiceForwardsUserID(t *testing.T) {
	store := &recordingStore{}
	service := NewService(store)
	if _, err := service.GetAI(context.Background(), 4, 15); err != nil {
		t.Fatalf("GetAI: %v", err)
	}
	if store.aiReads != 1 {
		t.Fatalf("aiReads = %d", store.aiReads)
	}
	if _, err := service.GetFinal(context.Background(), 4, 15); err != nil {
		t.Fatalf("GetFinal: %v", err)
	}
	if store.reads != 1 {
		t.Fatalf("reads = %d", store.reads)
	}
	value := Replacement{Marks: []initialread.Mark{}, CandidateDecisions: []Decision{}}
	if _, err := service.ReplaceFinal(context.Background(), 4, 15, value); err != nil {
		t.Fatalf("ReplaceFinal: %v", err)
	}
	if store.saves != 1 {
		t.Fatalf("saves = %d", store.saves)
	}
	result, err := service.Finalize(context.Background(), 4, 15)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if store.finalizes != 1 || result.Status != "" {
		t.Fatalf("finalizes = %d result = %+v", store.finalizes, result)
	}
	want := []int64{15, 15, 15, 15}
	if !reflect.DeepEqual(store.userSeen, want) {
		t.Fatalf("users passed to store = %v, want %v", store.userSeen, want)
	}
}

func TestFinalReadRejectsInvalidUser(t *testing.T) {
	for _, userID := range []int64{0, -1} {
		store := &recordingStore{}
		service := NewService(store)
		if _, err := service.GetAI(context.Background(), 4, userID); !errors.Is(err, ErrInvalid) {
			t.Fatalf("GetAI user %d error = %v, want ErrInvalid", userID, err)
		}
		if _, err := service.GetFinal(context.Background(), 4, userID); !errors.Is(err, ErrInvalid) {
			t.Fatalf("GetFinal user %d error = %v, want ErrInvalid", userID, err)
		}
		if _, err := service.ReplaceFinal(context.Background(), 4, userID, Replacement{Marks: []initialread.Mark{}, CandidateDecisions: []Decision{}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("ReplaceFinal user %d error = %v, want ErrInvalid", userID, err)
		}
		if _, err := service.Finalize(context.Background(), 4, userID); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Finalize user %d error = %v, want ErrInvalid", userID, err)
		}
		if len(store.userSeen) != 0 {
			t.Fatalf("invalid user %d reached the store: %v", userID, store.userSeen)
		}
	}
}

func TestFinalReadPropagatesAssignedToAnotherUser(t *testing.T) {
	store := &recordingStore{aiErr: fail(ErrAssignedToAnotherUser, "assigned")}
	service := NewService(store)
	if _, err := service.GetAI(context.Background(), 4, 15); !errors.Is(err, ErrAssignedToAnotherUser) {
		t.Fatalf("error = %v, want ErrAssignedToAnotherUser", err)
	}
}
