package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"nodule-reader/backend/internal/application/caseimport"
	"nodule-reader/backend/internal/infrastructure/upload"
)

const (
	// defaultMaxUploadRequestBytes caps one multipart upload, including the
	// multipart framing and any Content-Length lie.
	defaultMaxUploadRequestBytes = int64(2) << 30
	// maxConfirmBodyBytes caps the tiny confirm body.
	maxConfirmBodyBytes = 4 << 10
)

// uploadIDPattern accepts only a safe token charset, so a malformed path
// segment can never reach the session registry.
var uploadIDPattern = regexp.MustCompile(`^[0-9a-zA-Z_-]{1,64}$`)

// uploadService is the consumer-owned port the upload endpoints need. It is
// satisfied by *caseimport.Service.
type uploadService interface {
	Prepare(ctx context.Context, files []caseimport.StagedFile, skippedFiles int, userID int64) (caseimport.Session, error)
	Confirm(ctx context.Context, uploadID, seriesKey string, userID int64) (caseimport.Result, error)
	Get(ctx context.Context, uploadID string, userID int64) (caseimport.Session, error)
	Discard(ctx context.Context, uploadID string, userID int64) error
}

// uploadHandler serves the four upload endpoints. It is a self-dispatching
// handler registered on the upload namespace so no path-cleaning middleware can
// rewrite a rejected path before it is inspected.
type uploadHandler struct {
	service         uploadService
	uploadDir       string
	limits          upload.Limits
	maxRequestBytes int64
}

// NewUploadHandler builds the upload endpoints around the upload use case and
// the private workspace root.
func NewUploadHandler(service uploadService, uploadDir string) http.Handler {
	return &uploadHandler{
		service:         service,
		uploadDir:       uploadDir,
		limits:          upload.DefaultLimits(),
		maxRequestBytes: defaultMaxUploadRequestBytes,
	}
}

// ServeHTTP dispatches the upload namespace. Only the documented method/path
// pairs are accepted.
func (h *uploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/uploads")
	rest = strings.TrimPrefix(rest, "/")
	segments := []string{}
	if rest != "" {
		segments = strings.Split(rest, "/")
	}

	switch {
	case len(segments) == 0 && r.Method == http.MethodPost:
		h.create(w, r)
	case len(segments) == 1 && r.Method == http.MethodGet:
		h.get(w, r, segments[0])
	case len(segments) == 1 && r.Method == http.MethodDelete:
		h.discard(w, r, segments[0])
	case len(segments) == 2 && segments[1] == "cases" && r.Method == http.MethodPost:
		h.confirm(w, r, segments[0])
	default:
		h.writeError(w, http.StatusMethodNotAllowed, "INVALID_REQUEST")
	}
}

// create streams the multipart upload into a private workspace, examines it and
// returns the redacted session. No case, job or read exists yet.
func (h *uploadHandler) create(w http.ResponseWriter, r *http.Request) {
	if !h.checkOrigin(w, r) {
		return
	}
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if !isMultipart(r) {
		h.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_UPLOAD_FORMAT")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "INVALID_UPLOAD")
		return
	}

	workspace, err := upload.Stage(r.Context(), h.uploadDir, reader, h.limits)
	if err != nil {
		h.writeIntakeError(w, r, err)
		return
	}
	defer func() { _ = workspace.Cleanup() }()

	staged := workspace.Files()
	files := make([]caseimport.StagedFile, 0, len(staged))
	for _, file := range staged {
		files = append(files, caseimport.StagedFile{Path: file.Path, Size: file.Size})
	}
	session, err := h.service.Prepare(r.Context(), files, workspace.SkippedFiles(), principal.UserID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeSession(w, http.StatusCreated, session)
}

// get returns one session's redacted view so a client can reconcile a lost
// confirmation without resending any DICOM.
func (h *uploadHandler) get(w http.ResponseWriter, r *http.Request, id string) {
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if !validUploadID(id) {
		h.writeError(w, http.StatusNotFound, "UPLOAD_NOT_FOUND")
		return
	}
	session, err := h.service.Get(r.Context(), id, principal.UserID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeSession(w, http.StatusOK, session)
}

// confirm creates exactly one case for the selected series, or returns the
// existing one on a repeat.
func (h *uploadHandler) confirm(w http.ResponseWriter, r *http.Request, id string) {
	if !h.checkOrigin(w, r) {
		return
	}
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if !validUploadID(id) {
		h.writeError(w, http.StatusNotFound, "UPLOAD_NOT_FOUND")
		return
	}
	var payload struct {
		SeriesID string `json:"series_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxConfirmBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || payload.SeriesID == "" {
		h.writeError(w, http.StatusBadRequest, "INVALID_REQUEST")
		return
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		h.writeError(w, http.StatusBadRequest, "INVALID_REQUEST")
		return
	}

	result, err := h.service.Confirm(r.Context(), id, payload.SeriesID, principal.UserID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	status := http.StatusCreated
	if result.Existing {
		status = http.StatusOK
	}
	h.writeJSON(w, status, caseDTO{CaseID: result.CaseID, InstanceCount: result.InstanceCount, Existing: result.Existing})
}

// discard drops an unconfirmed session. It never deletes a case or any Orthanc
// data.
func (h *uploadHandler) discard(w http.ResponseWriter, r *http.Request, id string) {
	if !h.checkOrigin(w, r) {
		return
	}
	principal, ok := requirePrincipal(w, r)
	if !ok {
		return
	}
	if !validUploadID(id) {
		h.writeError(w, http.StatusNotFound, "UPLOAD_NOT_FOUND")
		return
	}
	if err := h.service.Discard(r.Context(), id, principal.UserID); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// checkOrigin enforces same-origin writes: a browser request that carries an
// Origin must match the public Host the proxy preserves. A request with no
// Origin (a native loopback client) is allowed.
func (h *uploadHandler) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, r.Host) {
		h.writeError(w, http.StatusForbidden, "ORIGIN_NOT_ALLOWED")
		return false
	}
	return true
}

// writeIntakeError maps an intake failure. A canceled request gets no response
// because the client is already gone.
func (h *uploadHandler) writeIntakeError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		h.writeError(w, http.StatusRequestEntityTooLarge, "UPLOAD_TOO_LARGE")
	case errors.Is(err, upload.ErrTooLarge),
		errors.Is(err, upload.ErrTooManyFiles),
		errors.Is(err, upload.ErrTooManyEntries):
		h.writeError(w, http.StatusRequestEntityTooLarge, "UPLOAD_TOO_LARGE")
	case errors.Is(err, upload.ErrMalformedArchive),
		errors.Is(err, upload.ErrNestedArchive),
		errors.Is(err, upload.ErrUnsupportedFormat):
		h.writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_UPLOAD_FORMAT")
	case errors.Is(err, upload.ErrUnknownField):
		h.writeError(w, http.StatusBadRequest, "INVALID_REQUEST")
	case errors.Is(err, upload.ErrUnsafeEntry):
		h.writeError(w, http.StatusBadRequest, "INVALID_UPLOAD")
	case errors.Is(err, upload.ErrCanceled), r.Context().Err() != nil:
		return
	default:
		h.writeError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
	}
}

// writeServiceError maps an application failure to the documented status and
// code. Cancellation gets no response.
func (h *uploadHandler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, caseimport.ErrCanceled), r.Context().Err() != nil:
		return
	case errors.Is(err, caseimport.ErrNotFound):
		h.writeError(w, http.StatusNotFound, "UPLOAD_NOT_FOUND")
	case errors.Is(err, caseimport.ErrForbidden):
		h.writeError(w, http.StatusForbidden, "UPLOAD_FORBIDDEN")
	case errors.Is(err, caseimport.ErrExpired):
		h.writeError(w, http.StatusConflict, "UPLOAD_EXPIRED")
	case errors.Is(err, caseimport.ErrAlreadyConfirmed):
		h.writeError(w, http.StatusConflict, "UPLOAD_ALREADY_CONFIRMED")
	case errors.Is(err, caseimport.ErrCommitOutcomeUnknown):
		h.writeError(w, http.StatusServiceUnavailable, "COMMIT_OUTCOME_UNKNOWN")
	case errors.Is(err, caseimport.ErrBusy):
		h.writeError(w, http.StatusTooManyRequests, "UPLOAD_BUSY")
	case errors.Is(err, caseimport.ErrCapacity):
		h.writeError(w, http.StatusTooManyRequests, "UPLOAD_CAPACITY")
	case errors.Is(err, caseimport.ErrUnsuitableSeries):
		h.writeError(w, http.StatusUnprocessableEntity, "UNSUITABLE_SERIES")
	case errors.Is(err, caseimport.ErrInvalidInput):
		h.writeError(w, http.StatusBadRequest, "INVALID_UPLOAD")
	case errors.Is(err, caseimport.ErrDuplicateSeries):
		h.writeError(w, http.StatusConflict, "UPLOAD_ALREADY_CONFIRMED")
	default:
		h.writeError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE")
	}
}

// writeSession renders the redacted session allowlist.
func (h *uploadHandler) writeSession(w http.ResponseWriter, status int, session caseimport.Session) {
	options := make([]seriesOptionDTO, 0, len(session.Series))
	for _, option := range session.Series {
		options = append(options, seriesOptionDTO{
			ID:            option.ID,
			Index:         option.Index,
			Modality:      option.Modality,
			InstanceCount: option.InstanceCount,
			Suitable:      option.Suitable,
			ReasonCode:    nullableString(option.ReasonCode),
		})
	}
	selected := nullableString(session.SelectedSeriesID)
	body := sessionDTO{
		ID:               session.ID,
		ExpiresAt:        session.ExpiresAt.UTC().Format(time.RFC3339),
		SkippedFiles:     session.SkippedFiles,
		Series:           options,
		SelectedSeriesID: selected,
		CaseID:           nullableInt64(session.CaseID),
	}
	h.writeJSON(w, status, body)
}

// seriesOptionDTO is the only series detail the API exposes before the initial
// read is locked: a session key, an index, modality, count and suitability.
type seriesOptionDTO struct {
	ID            string  `json:"id"`
	Index         int     `json:"index"`
	Modality      string  `json:"modality"`
	InstanceCount int     `json:"instance_count"`
	Suitable      bool    `json:"suitable"`
	ReasonCode    *string `json:"reason_code"`
}

// sessionDTO is the upload session allowlist. It never carries an Orthanc
// identifier, a UID, a path, a patient tag or an AI field.
type sessionDTO struct {
	ID               string            `json:"id"`
	ExpiresAt        string            `json:"expires_at"`
	SkippedFiles     int               `json:"skipped_files"`
	Series           []seriesOptionDTO `json:"series"`
	SelectedSeriesID *string           `json:"selected_series_id"`
	CaseID           *int64            `json:"case_id"`
}

// caseDTO is the confirm result.
type caseDTO struct {
	CaseID        int64 `json:"case_id"`
	InstanceCount int   `json:"instance_count"`
	Existing      bool  `json:"existing"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// uploadErrorMessages are the fixed, non-identifying messages. They never
// include a path, UID, credential, upstream body or database detail.
var uploadErrorMessages = map[string]string{
	"INVALID_REQUEST":           "Yêu cầu tải lên không hợp lệ.",
	"INVALID_UPLOAD":            "Bộ dữ liệu tải lên không hợp lệ.",
	"ORIGIN_NOT_ALLOWED":        "Yêu cầu không xuất phát từ trang hợp lệ.",
	"UPLOAD_NOT_FOUND":          "Không tìm thấy phiên tải lên này.",
	"UPLOAD_FORBIDDEN":          "Phiên tải lên này thuộc về người dùng khác.",
	"UPLOAD_EXPIRED":            "Phiên tải lên đã hết hạn; cần tải lại dữ liệu.",
	"UPLOAD_ALREADY_CONFIRMED":  "Phiên tải lên đã được xác nhận với series khác.",
	"UPLOAD_TOO_LARGE":          "Bộ dữ liệu tải lên vượt quá giới hạn cho phép.",
	"UNSUPPORTED_UPLOAD_FORMAT": "Định dạng tải lên không được hỗ trợ.",
	"UNSUITABLE_SERIES":         "Series đã chọn không phù hợp để mở ca.",
	"UPLOAD_BUSY":               "Đang có một lượt tải lên khác; thử lại sau.",
	"UPLOAD_CAPACITY":           "Đã đạt số phiên tải lên tối đa; thử lại sau.",
	"DEPENDENCY_UNAVAILABLE":    "Dịch vụ tạm thời không khả dụng. Thử lại sau.",
	"COMMIT_OUTCOME_UNKNOWN":    "Chưa xác định được kết quả tạo ca; cần kiểm tra lại.",
}

// writeError renders the fixed error envelope with no-store.
func (h *uploadHandler) writeError(w http.ResponseWriter, status int, code string) {
	message, ok := uploadErrorMessages[code]
	if !ok {
		message = uploadErrorMessages["DEPENDENCY_UNAVAILABLE"]
	}
	h.writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

func (h *uploadHandler) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// isMultipart reports whether the request declares a multipart body.
func isMultipart(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "multipart/form-data"
}

func validUploadID(id string) bool { return uploadIDPattern.MatchString(id) }

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nullableInt64(value int64) *int64 {
	if value == 0 {
		return nil
	}
	return &value
}
