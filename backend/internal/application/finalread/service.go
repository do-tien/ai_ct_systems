package finalread

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nodule-reader/backend/internal/application/aijob"
	"nodule-reader/backend/internal/application/initialread"
)

// Service is the LC1-13 use case: the post-lock AI projection, the editable
// final read and POC finalization. It depends only on the Store port.
type Service struct {
	store Store
}

// NewService builds the use case from its persistence port.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// GetAI returns the state-specific AI projection of the case. Before the initial
// read is locked the adapter returns ErrForbidden; the service never touches AI
// tables itself.
func (s *Service) GetAI(ctx context.Context, caseID int64) (AI, error) {
	return s.store.GetAI(ctx, caseID, initialread.Reader)
}

// GetFinal returns the editable final read without creating or copying one.
func (s *Service) GetFinal(ctx context.Context, caseID int64) (Read, error) {
	return s.store.GetFinal(ctx, caseID, initialread.Reader)
}

// ReplaceFinal validates the whole replacement and then persists both lists
// atomically. Nothing is persisted unless every mark and decision is valid;
// candidate membership is enforced by the transactional store.
func (s *Service) ReplaceFinal(ctx context.Context, caseID int64, value Replacement) (Read, error) {
	if err := validateReplacement(value); err != nil {
		return Read{}, err
	}
	return s.store.ReplaceFinal(ctx, caseID, initialread.Reader, value)
}

// Finalize completes the case without waiting for AI to finish.
func (s *Service) Finalize(ctx context.Context, caseID int64) (FinalizeResult, error) {
	return s.store.Finalize(ctx, caseID, initialread.Reader)
}

// validateReplacement rejects the whole replacement on the first invalid member.
// An absent or null list is rejected; an empty list is a valid clear. A
// candidate-derived mark must be linked to an accepted decision supplied in the
// same replacement, and no candidate may be linked twice. A decision without a
// linked mark is valid: the decision and the measurement are separate data.
func validateReplacement(value Replacement) error {
	if value.Marks == nil {
		return fail(ErrInvalid, "marks list is required")
	}
	if value.CandidateDecisions == nil {
		return fail(ErrInvalid, "candidate decisions list is required")
	}

	accepted := make(map[string]struct{}, len(value.CandidateDecisions))
	decided := make(map[string]struct{}, len(value.CandidateDecisions))
	for _, decision := range value.CandidateDecisions {
		if strings.TrimSpace(decision.CandidateID) == "" {
			return fail(ErrInvalid, "decision candidate id is required")
		}
		if _, duplicate := decided[decision.CandidateID]; duplicate {
			return fail(ErrInvalid, "candidate decisions must be unique")
		}
		decided[decision.CandidateID] = struct{}{}

		switch decision.Decision {
		case DecisionAccepted:
			if decision.DismissReason != nil {
				return fail(ErrInvalid, "an accepted decision carries no reason")
			}
			accepted[decision.CandidateID] = struct{}{}
		case DecisionDismissed:
			if decision.DismissReason == nil || strings.TrimSpace(*decision.DismissReason) == "" {
				return fail(ErrInvalid, "a dismissed decision requires a reason")
			}
		default:
			return fail(ErrInvalid, "decision must be accepted or dismissed")
		}
	}

	derived := make(map[string]struct{})
	for _, mark := range value.Marks {
		if err := validateFinalMark(mark); err != nil {
			return err
		}
		if mark.Origin != OriginFromAICandidate {
			continue
		}
		candidateID := *mark.AICandidateID
		if _, duplicate := derived[candidateID]; duplicate {
			return fail(ErrInvalid, "a candidate may be linked at most once")
		}
		derived[candidateID] = struct{}{}
		if _, ok := accepted[candidateID]; !ok {
			return fail(ErrInvalid, "a candidate-derived mark requires an accepted decision")
		}
	}
	return nil
}

// validateFinalMark checks one final mark: its origin/candidate coupling and the
// shared measurement geometry. Geometry failures are converted to this package's
// ErrInvalid rather than leaking the initial-read error category.
func validateFinalMark(mark initialread.Mark) error {
	switch mark.Origin {
	case OriginReader, OriginReaderAfterAI:
		if mark.AICandidateID != nil {
			return fail(ErrInvalid, "only candidate-derived marks carry a candidate id")
		}
	case OriginFromAICandidate:
		if mark.AICandidateID == nil || strings.TrimSpace(*mark.AICandidateID) == "" {
			return fail(ErrInvalid, "a candidate-derived mark requires a candidate id")
		}
	default:
		return fail(ErrInvalid, "mark origin is not accepted in the final read")
	}
	if err := initialread.ValidateGeometry(mark); err != nil {
		return fail(ErrInvalid, "measurement geometry is invalid")
	}
	return nil
}

// ProjectAI converts one stored job snapshot into the allowlisted AI projection.
// The caller owns the raw result bytes; only a succeeded job decoded against the
// real case linkage is projected. A corrupt success document returns an error
// the caller classifies; no partial candidate is ever published.
func ProjectAI(status string, errorCode *string, result []byte, seriesInstanceUID, frameOfReferenceUID string) (AI, error) {
	switch status {
	case AIQueued, AIRunning:
		// Running never implies that candidates are revealed.
		return AI{Status: status}, nil
	case AIFailed:
		code := AIUnavailableCode
		if errorCode != nil && knownAIErrorCode(*errorCode) {
			code = *errorCode
		}
		return AI{
			Status:     AIFailed,
			Candidates: []Candidate{},
			Error:      &AIError{Code: code, Message: AIUnavailableMessage},
		}, nil
	case AISucceeded:
		job := aijob.Job{SeriesInstanceUID: seriesInstanceUID, FrameOfReferenceUID: frameOfReferenceUID}
		if err := aijob.ValidateSuccess(json.RawMessage(result), job); err != nil {
			return AI{}, fmt.Errorf("stored AI result is invalid: %w", err)
		}
		return decodeSuccess(result)
	default:
		return AI{}, fmt.Errorf("unknown AI job status")
	}
}

// decodeSuccess decodes a result document that ProjectAI already validated
// against the LC1-12 contract into the typed, allowlisted projection. Unknown
// members are dropped by the explicit struct shape.
func decodeSuccess(result []byte) (AI, error) {
	var document struct {
		Model struct {
			Bundle         string  `json:"bundle"`
			BundleVersion  string  `json:"bundle_version"`
			ScoreThreshold float64 `json:"score_threshold"`
		} `json:"model"`
		Input struct {
			NumSlices  int       `json:"num_slices"`
			SpacingMM  []float64 `json:"spacing_mm"`
			SizeVoxels []int     `json:"size_voxels"`
		} `json:"input"`
		Candidates []struct {
			ID       string    `json:"id"`
			CenterMM []float64 `json:"center_mm"`
			SizeMM   []float64 `json:"size_mm"`
			Score    float64   `json:"score"`
		} `json:"candidates"`
		Timing struct {
			Load      float64 `json:"load"`
			Inference float64 `json:"inference"`
			Total     float64 `json:"total"`
		} `json:"timing_s"`
		Resources struct {
			Device string `json:"device"`
		} `json:"resources"`
	}
	decoder := json.NewDecoder(bytes.NewReader(result))
	if err := decoder.Decode(&document); err != nil {
		return AI{}, fmt.Errorf("stored AI result cannot be decoded: %w", err)
	}

	candidates := make([]Candidate, 0, len(document.Candidates))
	for _, entry := range document.Candidates {
		candidates = append(candidates, Candidate{
			ID:       entry.ID,
			CenterMM: entry.CenterMM,
			SizeMM:   entry.SizeMM,
			Score:    entry.Score,
		})
	}
	return AI{
		Status:     AISucceeded,
		Candidates: candidates,
		Model: &AIModel{
			Bundle:         document.Model.Bundle,
			BundleVersion:  document.Model.BundleVersion,
			ScoreThreshold: document.Model.ScoreThreshold,
		},
		Input: &AIInput{
			NumSlices:  document.Input.NumSlices,
			SpacingMM:  document.Input.SpacingMM,
			SizeVoxels: document.Input.SizeVoxels,
		},
		Timing: &AITiming{
			Load:      document.Timing.Load,
			Inference: document.Timing.Inference,
			Total:     document.Timing.Total,
		},
		Resources: &AIResources{Device: document.Resources.Device},
	}, nil
}

// orchestrationCodes is the LC1-12 infrastructure code set. Together with the
// inference CLI's safe code set it forms the allowlist a failed job may project.
var orchestrationCodes = map[string]struct{}{
	aijob.CodeOrthancArchiveError: {},
	aijob.CodeArchiveInvalid:      {},
	aijob.CodeWorkspaceError:      {},
	aijob.CodeProcessStartFailed:  {},
	aijob.CodeAITimeout:           {},
	aijob.CodeAICanceled:          {},
	aijob.CodeResultMissing:       {},
	aijob.CodeResultInvalid:       {},
	aijob.CodeProcessError:        {},
}

// knownAIErrorCode reports whether a stored failure code belongs to the fixed
// allowlist. Any other code, including null, must project as AI_UNAVAILABLE.
func knownAIErrorCode(code string) bool {
	if _, ok := orchestrationCodes[code]; ok {
		return true
	}
	return aijob.IsCLISafeCode(code)
}
