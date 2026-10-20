package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"nodule-reader/backend/internal/application/initialread"
)

// Stable error codes and their fixed, non-identifying messages. The transport
// never echoes SQL, network, credential or adapter text.
const (
	codeInvalidRequest      = "INVALID_REQUEST"
	codeCaseNotFound        = "CASE_NOT_FOUND"
	codeInitialReadLocked   = "INITIAL_READ_LOCKED"
	codeInitialReadConflict = "INITIAL_READ_CONFLICT"
	codeRequestTooLarge     = "REQUEST_TOO_LARGE"
	codeUnsupportedMedia    = "UNSUPPORTED_MEDIA_TYPE"
	codeDependencyAvailable = "DEPENDENCY_UNAVAILABLE"
	codeCommitUnknown       = "COMMIT_OUTCOME_UNKNOWN"
	codeInternalError       = "INTERNAL_ERROR"
	codeAuthRequired        = "AUTH_REQUIRED"
	codeInvalidCredentials  = "INVALID_CREDENTIALS"
	codeTooManyAttempts     = "TOO_MANY_ATTEMPTS"
	codeOriginNotAllowed    = "ORIGIN_NOT_ALLOWED"
)

// maxMarksBodyBytes bounds one marks replacement body.
const maxMarksBodyBytes = 1 << 20

var apiErrorMessages = map[string]string{
	codeInvalidRequest:      "request is invalid",
	codeCaseNotFound:        "case not found",
	codeInitialReadLocked:   "initial read is locked",
	codeInitialReadConflict: "initial read state conflicts with submission",
	codeInitialReadRequired: "the independent initial read is not locked",
	codeFinalReadLocked:     "the final read is locked",
	codeFinalReadConflict:   "final read state conflicts with the request",
	codeRequestTooLarge:     "request body is too large",
	codeUnsupportedMedia:    "content type must be application/json",
	codeDependencyAvailable: "storage is unavailable",
	codeCommitUnknown:       "commit outcome is unknown; reload before retrying",
	codeInternalError:       "internal error",
	codeAuthRequired:        "authentication is required",
	codeInvalidCredentials:  "credentials are invalid",
	codeTooManyAttempts:     "too many authentication attempts; retry later",
	codeOriginNotAllowed:    "request origin is not allowed",
}

type apiErrorEnvelope struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// initialReadHandler owns the three LC1-10 routes. It maps the use case to
// JSON/HTTP and never serializes database rows.
type initialReadHandler struct {
	service *initialread.Service
}

// caseResponse is the allowlisted case projection. orthanc_series_id and every
// AI field are deliberately absent.
type caseResponse struct {
	ID                  int64      `json:"id"`
	Source              string     `json:"source"`
	StudyInstanceUID    string     `json:"study_instance_uid"`
	SeriesInstanceUID   string     `json:"series_instance_uid"`
	FrameOfReferenceUID string     `json:"frame_of_reference_uid"`
	Status              string     `json:"status"`
	CreatedAt           time.Time  `json:"created_at"`
	FinalizedAt         *time.Time `json:"finalized_at"`
}

type readResponse struct {
	ID       int64          `json:"id"`
	CaseID   int64          `json:"case_id"`
	Kind     string         `json:"kind"`
	Reader   string         `json:"reader"`
	LockedAt *time.Time     `json:"locked_at"`
	Marks    []markResponse `json:"marks"`
}

// markResponse is one saved measurement. It never carries database IDs.
type markResponse struct {
	P1MM            []float64 `json:"p1_mm"`
	P2MM            []float64 `json:"p2_mm"`
	CenterMM        []float64 `json:"center_mm"`
	DiameterMM      float64   `json:"diameter_mm"`
	Origin          string    `json:"origin"`
	AICandidateID   *string   `json:"ai_candidate_id"`
	ViewPlaneNormal []float64 `json:"view_plane_normal"`
	Note            *string   `json:"note"`
}

// submitResponse is the committed submission outcome. It carries only the new
// status, the database lock timestamp and the final read ID.
type submitResponse struct {
	Status      string    `json:"status"`
	LockedAt    time.Time `json:"locked_at"`
	FinalReadID int64     `json:"final_read_id"`
}

// marksPayload is the PUT body. Unknown fields are rejected by the decoder.
type marksPayload struct {
	Marks []markInput `json:"marks"`
}

// markInput uses pointer and pointer-element fields so an absent or explicit
// null coordinate is distinguishable from a real zero and can be rejected.
type markInput struct {
	P1MM            []*float64 `json:"p1_mm"`
	P2MM            []*float64 `json:"p2_mm"`
	CenterMM        []*float64 `json:"center_mm"`
	DiameterMM      *float64   `json:"diameter_mm"`
	Origin          *string    `json:"origin"`
	AICandidateID   *string    `json:"ai_candidate_id"`
	ViewPlaneNormal []*float64 `json:"view_plane_normal"`
	Note            *string    `json:"note"`
}

func (h *initialReadHandler) getCase(writer http.ResponseWriter, request *http.Request) {
	caseID, ok := caseIDFromRequest(request)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	value, err := h.service.GetCase(request.Context(), caseID)
	if err != nil {
		writeAPIError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, caseResponse{
		ID:                  value.ID,
		Source:              value.Source,
		StudyInstanceUID:    value.StudyInstanceUID,
		SeriesInstanceUID:   value.SeriesInstanceUID,
		FrameOfReferenceUID: value.FrameOfReferenceUID,
		Status:              value.Status,
		CreatedAt:           value.CreatedAt,
		FinalizedAt:         value.FinalizedAt,
	})
}

func (h *initialReadHandler) getInitial(writer http.ResponseWriter, request *http.Request) {
	caseID, ok := caseIDFromRequest(request)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	read, err := h.service.GetInitial(request.Context(), caseID)
	if err != nil {
		writeAPIError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, readResponse{
		ID:       read.ID,
		CaseID:   read.CaseID,
		Kind:     read.Kind,
		Reader:   read.Reader,
		LockedAt: read.LockedAt,
		Marks:    markResponses(read.Marks),
	})
}

func (h *initialReadHandler) putMarks(writer http.ResponseWriter, request *http.Request) {
	caseID, ok := caseIDFromRequest(request)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		writeAPIErrorStatus(writer, http.StatusUnsupportedMediaType, codeUnsupportedMedia)
		return
	}

	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxMarksBodyBytes))
	decoder.DisallowUnknownFields()

	var payload marksPayload
	if err := decoder.Decode(&payload); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIErrorStatus(writer, http.StatusRequestEntityTooLarge, codeRequestTooLarge)
			return
		}
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	if payload.Marks == nil {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}

	marks, ok := decodeMarks(payload.Marks)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}

	saved, err := h.service.ReplaceMarks(request.Context(), caseID, marks)
	if err != nil {
		writeAPIError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, markResponses(saved))
}

// submit locks the initial read and creates its final copy. The request has no
// payload: any nonempty body is rejected without being interpreted.
func (h *initialReadHandler) submit(writer http.ResponseWriter, request *http.Request) {
	caseID, ok := caseIDFromRequest(request)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	bodyEmpty, err := requestBodyEmpty(request)
	if err != nil || !bodyEmpty {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}

	result, err := h.service.SubmitInitial(request.Context(), caseID)
	if err != nil {
		writeAPIError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, submitResponse{
		Status:      result.Status,
		LockedAt:    result.LockedAt,
		FinalReadID: result.FinalReadID,
	})
}

// requestBodyEmpty reports whether the request body carries no byte. The single
// bounded read also covers chunked bodies, which have no declared length.
func requestBodyEmpty(request *http.Request) (bool, error) {
	if request.Body == nil {
		return true, nil
	}
	buffer := make([]byte, 1)
	read, err := request.Body.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return read == 0, nil
}

// decodeMarks builds application marks from the wire form. It rejects any
// absent vector, null vector element or absent scalar so a missing coordinate
// can never become a zero-valued triple.
func decodeMarks(inputs []markInput) ([]initialread.Mark, bool) {
	marks := make([]initialread.Mark, 0, len(inputs))
	for _, input := range inputs {
		mark, ok := input.toMark()
		if !ok {
			return nil, false
		}
		marks = append(marks, mark)
	}
	return marks, true
}

func (input markInput) toMark() (initialread.Mark, bool) {
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
	normal, ok := decodeVector(input.ViewPlaneNormal)
	if !ok {
		return initialread.Mark{}, false
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

func decodeVector(values []*float64) ([]float64, bool) {
	if values == nil {
		return nil, false
	}
	out := make([]float64, len(values))
	for index, value := range values {
		if value == nil {
			return nil, false
		}
		out[index] = *value
	}
	return out, true
}

func markResponses(marks []initialread.Mark) []markResponse {
	out := make([]markResponse, 0, len(marks))
	for _, mark := range marks {
		out = append(out, markResponse{
			P1MM:            mark.P1MM,
			P2MM:            mark.P2MM,
			CenterMM:        mark.CenterMM,
			DiameterMM:      mark.DiameterMM,
			Origin:          mark.Origin,
			AICandidateID:   mark.AICandidateID,
			ViewPlaneNormal: mark.ViewPlaneNormal,
			Note:            mark.Note,
		})
	}
	return out
}

func caseIDFromRequest(request *http.Request) (int64, bool) {
	raw := chi.URLParam(request, "id")
	caseID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || caseID <= 0 {
		return 0, false
	}
	return caseID, true
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func writeAPIError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, initialread.ErrInvalid):
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
	case errors.Is(err, initialread.ErrNotFound):
		writeAPIErrorStatus(writer, http.StatusNotFound, codeCaseNotFound)
	case errors.Is(err, initialread.ErrLocked):
		writeAPIErrorStatus(writer, http.StatusConflict, codeInitialReadLocked)
	case errors.Is(err, initialread.ErrConflict):
		writeAPIErrorStatus(writer, http.StatusConflict, codeInitialReadConflict)
	case errors.Is(err, initialread.ErrCommitOutcomeUnknown):
		writeAPIErrorStatus(writer, http.StatusServiceUnavailable, codeCommitUnknown)
	case errors.Is(err, initialread.ErrDependency):
		writeAPIErrorStatus(writer, http.StatusServiceUnavailable, codeDependencyAvailable)
	default:
		writeAPIErrorStatus(writer, http.StatusInternalServerError, codeInternalError)
	}
}

func writeAPIErrorStatus(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, apiErrorEnvelope{Error: apiError{Code: code, Message: apiErrorMessages[code]}})
}

// writeJSON sets the shared API headers and encodes the value. Cache-Control is
// no-store on every response so a browser never caches clinical or AI-adjacent
// data.
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
