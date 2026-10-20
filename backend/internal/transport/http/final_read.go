package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"nodule-reader/backend/internal/application/finalread"
	"nodule-reader/backend/internal/application/initialread"
)

// Additional stable error codes for the post-lock workflow. Messages are fixed
// and never echo SQL, network, credential or adapter text.
const (
	codeInitialReadRequired = "INITIAL_READ_REQUIRED"
	codeFinalReadLocked     = "FINAL_READ_LOCKED"
	codeFinalReadConflict   = "FINAL_READ_CONFLICT"
)

// finalReadHandler owns the four LC1-13 routes. It maps the use case to
// JSON/HTTP and never serializes database rows or raw result bytes.
type finalReadHandler struct {
	service *finalread.Service
}

// decisionResponse is one physician decision. It never carries database IDs or
// timestamps.
type decisionResponse struct {
	CandidateID   string  `json:"candidate_id"`
	Decision      string  `json:"decision"`
	DismissReason *string `json:"dismiss_reason"`
}

type finalReadResponse struct {
	ID                 int64              `json:"id"`
	CaseID             int64              `json:"case_id"`
	Kind               string             `json:"kind"`
	Reader             string             `json:"reader"`
	LockedAt           *time.Time         `json:"locked_at"`
	Marks              []markResponse     `json:"marks"`
	CandidateDecisions []decisionResponse `json:"candidate_decisions"`
}

type finalizeResponse struct {
	Status      string    `json:"status"`
	FinalizedAt time.Time `json:"finalized_at"`
	AIStatus    string    `json:"ai_status"`
}

type candidateResponse struct {
	ID       string    `json:"id"`
	CenterMM []float64 `json:"center_mm"`
	SizeMM   []float64 `json:"size_mm"`
	Score    float64   `json:"score"`
}

type aiErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type aiModelResponse struct {
	Bundle         string  `json:"bundle"`
	BundleVersion  string  `json:"bundle_version"`
	ScoreThreshold float64 `json:"score_threshold"`
}

type aiInputResponse struct {
	NumSlices  int       `json:"num_slices"`
	SpacingMM  []float64 `json:"spacing_mm"`
	SizeVoxels []int     `json:"size_voxels"`
}

type aiTimingResponse struct {
	Load      float64 `json:"load"`
	Inference float64 `json:"inference"`
	Total     float64 `json:"total"`
}

type aiResourcesResponse struct {
	Device string `json:"device"`
}

// aiBaseResponse is the projection of every non-succeeded state. Candidates is
// null while queued/running and empty while failed; error is always present and
// null unless the job failed.
type aiBaseResponse struct {
	Status     string              `json:"status"`
	Candidates []candidateResponse `json:"candidates"`
	Error      *aiErrorResponse    `json:"error"`
}

// aiSuccessResponse is the projection of a succeeded job. It carries no error
// member.
type aiSuccessResponse struct {
	Status     string              `json:"status"`
	Candidates []candidateResponse `json:"candidates"`
	Model      aiModelResponse     `json:"model"`
	Input      aiInputResponse     `json:"input"`
	Timing     aiTimingResponse    `json:"timing_s"`
	Resources  aiResourcesResponse `json:"resources"`
}

// finalPayload is the PUT body. Unknown fields are rejected by the decoder.
type finalPayload struct {
	Marks              []markInput     `json:"marks"`
	CandidateDecisions []decisionInput `json:"candidate_decisions"`
}

type decisionInput struct {
	CandidateID   *string `json:"candidate_id"`
	Decision      *string `json:"decision"`
	DismissReason *string `json:"dismiss_reason"`
}

func (h *finalReadHandler) getAI(writer http.ResponseWriter, request *http.Request) {
	caseID, ok := caseIDFromRequest(request)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	principal, ok := requirePrincipal(writer, request)
	if !ok {
		return
	}
	value, err := h.service.GetAI(request.Context(), caseID, principal.UserID)
	if err != nil {
		writeFinalReadError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, aiResponse(value))
}

func (h *finalReadHandler) getFinal(writer http.ResponseWriter, request *http.Request) {
	caseID, ok := caseIDFromRequest(request)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	principal, ok := requirePrincipal(writer, request)
	if !ok {
		return
	}
	read, err := h.service.GetFinal(request.Context(), caseID, principal.UserID)
	if err != nil {
		writeFinalReadError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, finalReadResponseOf(read))
}

// putFinal replaces the whole final read. Access and state are preflighted
// before the body is interpreted, so an awaiting case returns 403 and a finalized
// case returns 409 without inspecting AI or payload content; the store repeats
// every authoritative check inside its transaction.
func (h *finalReadHandler) putFinal(writer http.ResponseWriter, request *http.Request) {
	caseID, ok := caseIDFromRequest(request)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}

	principal, ok := requirePrincipal(writer, request)
	if !ok {
		return
	}

	preflight, err := h.service.GetFinal(request.Context(), caseID, principal.UserID)
	if err != nil {
		writeFinalReadError(writer, err)
		return
	}
	if preflight.LockedAt != nil {
		writeAPIErrorStatus(writer, http.StatusConflict, codeFinalReadLocked)
		return
	}

	if !isJSONContentType(request.Header.Get("Content-Type")) {
		writeAPIErrorStatus(writer, http.StatusUnsupportedMediaType, codeUnsupportedMedia)
		return
	}

	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxMarksBodyBytes))
	decoder.DisallowUnknownFields()
	var payload finalPayload
	if err := decoder.Decode(&payload); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIErrorStatus(writer, http.StatusRequestEntityTooLarge, codeRequestTooLarge)
			return
		}
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	if payload.Marks == nil || payload.CandidateDecisions == nil {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}

	replacement, ok := decodeReplacement(payload)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}

	read, err := h.service.ReplaceFinal(request.Context(), caseID, principal.UserID, replacement)
	if err != nil {
		writeFinalReadError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, finalReadResponseOf(read))
}

// finalize completes the case. The request has no payload: any nonempty body is
// rejected without being interpreted.
func (h *finalReadHandler) finalize(writer http.ResponseWriter, request *http.Request) {
	caseID, ok := caseIDFromRequest(request)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	principal, ok := requirePrincipal(writer, request)
	if !ok {
		return
	}
	bodyEmpty, err := requestBodyEmpty(request)
	if err != nil || !bodyEmpty {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}

	result, err := h.service.Finalize(request.Context(), caseID, principal.UserID)
	if err != nil {
		writeFinalReadError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, finalizeResponse{
		Status:      result.Status,
		FinalizedAt: result.FinalizedAt,
		AIStatus:    result.AIStatus,
	})
}

// decodeReplacement builds the application replacement from the wire form,
// rejecting any absent mark vector element, scalar or decision member.
func decodeReplacement(payload finalPayload) (finalread.Replacement, bool) {
	marks := make([]initialread.Mark, 0, len(payload.Marks))
	for _, input := range payload.Marks {
		mark, ok := input.toFinalMark()
		if !ok {
			return finalread.Replacement{}, false
		}
		marks = append(marks, mark)
	}
	decisions := make([]finalread.Decision, 0, len(payload.CandidateDecisions))
	for _, input := range payload.CandidateDecisions {
		if input.CandidateID == nil || input.Decision == nil {
			return finalread.Replacement{}, false
		}
		decisions = append(decisions, finalread.Decision{
			CandidateID:   *input.CandidateID,
			Decision:      *input.Decision,
			DismissReason: input.DismissReason,
		})
	}
	return finalread.Replacement{Marks: marks, CandidateDecisions: decisions}, true
}

// toFinalMark builds a final mark. Unlike the initial read, a nil view-plane
// normal is valid; a supplied one must still be three finite numbers.
func (input markInput) toFinalMark() (initialread.Mark, bool) {
	p1, ok := decodeVector(input.P1MM)
	if !ok {
		return initialread.Mark{}, false
	}
	p2, ok := decodeVector(input.P2MM)
	if !ok {
		return initialread.Mark{}, false
	}
	center, ok := decodeVector(input.CenterMM)
	if !ok {
		return initialread.Mark{}, false
	}
	var normal []float64
	if input.ViewPlaneNormal != nil {
		normal, ok = decodeVector(input.ViewPlaneNormal)
		if !ok {
			return initialread.Mark{}, false
		}
	}
	if input.DiameterMM == nil || input.Origin == nil {
		return initialread.Mark{}, false
	}
	return initialread.Mark{
		P1MM:            p1,
		P2MM:            p2,
		CenterMM:        center,
		DiameterMM:      *input.DiameterMM,
		Origin:          *input.Origin,
		AICandidateID:   input.AICandidateID,
		ViewPlaneNormal: normal,
		Note:            input.Note,
	}, true
}

// aiResponse selects the state-specific projection so a succeeded document
// never carries an error member and every other state carries the fixed triple.
func aiResponse(value finalread.AI) any {
	if value.Model != nil {
		return aiSuccessResponse{
			Status:     value.Status,
			Candidates: candidateResponses(value.Candidates),
			Model: aiModelResponse{
				Bundle:         value.Model.Bundle,
				BundleVersion:  value.Model.BundleVersion,
				ScoreThreshold: value.Model.ScoreThreshold,
			},
			Input: aiInputResponse{
				NumSlices:  value.Input.NumSlices,
				SpacingMM:  value.Input.SpacingMM,
				SizeVoxels: value.Input.SizeVoxels,
			},
			Timing: aiTimingResponse{
				Load:      value.Timing.Load,
				Inference: value.Timing.Inference,
				Total:     value.Timing.Total,
			},
			Resources: aiResourcesResponse{Device: value.Resources.Device},
		}
	}
	response := aiBaseResponse{Status: value.Status, Candidates: candidateResponses(value.Candidates)}
	if value.Error != nil {
		response.Error = &aiErrorResponse{Code: value.Error.Code, Message: value.Error.Message}
	}
	return response
}

func candidateResponses(candidates []finalread.Candidate) []candidateResponse {
	if candidates == nil {
		return nil
	}
	out := make([]candidateResponse, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidateResponse{
			ID:       candidate.ID,
			CenterMM: candidate.CenterMM,
			SizeMM:   candidate.SizeMM,
			Score:    candidate.Score,
		})
	}
	return out
}

func finalReadResponseOf(read finalread.Read) finalReadResponse {
	decisions := make([]decisionResponse, 0, len(read.CandidateDecisions))
	for _, decision := range read.CandidateDecisions {
		decisions = append(decisions, decisionResponse{
			CandidateID:   decision.CandidateID,
			Decision:      decision.Decision,
			DismissReason: decision.DismissReason,
		})
	}
	return finalReadResponse{
		ID:                 read.ID,
		CaseID:             read.CaseID,
		Kind:               read.Kind,
		Reader:             read.Reader,
		LockedAt:           read.LockedAt,
		Marks:              markResponses(read.Marks),
		CandidateDecisions: decisions,
	}
}

// writeFinalReadError maps the LC1-13 categories to statuses without leaking
// adapter detail.
func writeFinalReadError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, finalread.ErrInvalid):
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
	case errors.Is(err, finalread.ErrNotFound):
		writeAPIErrorStatus(writer, http.StatusNotFound, codeCaseNotFound)
	case errors.Is(err, finalread.ErrForbidden):
		writeAPIErrorStatus(writer, http.StatusForbidden, codeInitialReadRequired)
	case errors.Is(err, finalread.ErrAssignedToAnotherUser):
		writeAPIErrorStatus(writer, http.StatusForbidden, codeCaseAssigned)
	case errors.Is(err, finalread.ErrLocked):
		writeAPIErrorStatus(writer, http.StatusConflict, codeFinalReadLocked)
	case errors.Is(err, finalread.ErrConflict):
		writeAPIErrorStatus(writer, http.StatusConflict, codeFinalReadConflict)
	case errors.Is(err, finalread.ErrCommitOutcomeUnknown):
		writeAPIErrorStatus(writer, http.StatusServiceUnavailable, codeCommitUnknown)
	case errors.Is(err, finalread.ErrDependency):
		writeAPIErrorStatus(writer, http.StatusServiceUnavailable, codeDependencyAvailable)
	default:
		writeAPIErrorStatus(writer, http.StatusInternalServerError, codeInternalError)
	}
}
