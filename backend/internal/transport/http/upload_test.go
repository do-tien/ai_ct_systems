package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"nodule-reader/backend/internal/application/caseimport"
	"nodule-reader/backend/internal/infrastructure/upload"
)

// fakeUploadService is a scripted upload port. It records every call so a test
// can prove the handler staged real files and forwarded the skip count.
type fakeUploadService struct {
	prepareResult  caseimport.Session
	prepareErr     error
	confirmResult  caseimport.Result
	confirmErr     error
	getResult      caseimport.Session
	getErr         error
	discardErr     error
	prepareFiles   []caseimport.StagedFile
	prepareSkipped int
	confirmKey     string
	confirmID      string
}

func (f *fakeUploadService) Prepare(ctx context.Context, files []caseimport.StagedFile, skipped int) (caseimport.Session, error) {
	f.prepareFiles = files
	f.prepareSkipped = skipped
	return f.prepareResult, f.prepareErr
}

func (f *fakeUploadService) Confirm(ctx context.Context, uploadID, seriesKey string) (caseimport.Result, error) {
	f.confirmID, f.confirmKey = uploadID, seriesKey
	return f.confirmResult, f.confirmErr
}

func (f *fakeUploadService) Get(ctx context.Context, uploadID string) (caseimport.Session, error) {
	return f.getResult, f.getErr
}

func (f *fakeUploadService) Discard(ctx context.Context, uploadID string) error {
	return f.discardErr
}

type uploadPart struct {
	name  string
	data  []byte
	field string
}

func uploadRequest(t *testing.T, method, target string, parts []uploadPart) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, p := range parts {
		field := p.field
		if field == "" {
			field = "files"
		}
		header := textproto.MIMEHeader{
			"Content-Disposition": []string{`form-data; name="` + field + `"; filename="` + p.name + `"`},
		}
		w, err := writer.CreatePart(header)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := w.Write(p.data); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	request := httptest.NewRequest(method, target, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func uploadRouter(t *testing.T, service uploadService, limits upload.Limits, maxBytes int64) http.Handler {
	t.Helper()
	handler := &uploadHandler{
		service:         service,
		uploadDir:       t.TempDir(),
		limits:          limits,
		maxRequestBytes: maxBytes,
	}
	return NewRouter(nil, nil, nil, handler, http.NotFoundHandler())
}

func doUpload(t *testing.T, router http.Handler, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return payload
}

func uploadErrorCode(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %v", payload)
	}
	code, ok := raw["code"].(string)
	if !ok {
		t.Fatalf("no error code in %v", raw)
	}
	return code
}

func sampleSession() caseimport.Session {
	return caseimport.Session{
		ID:           "session-token",
		ExpiresAt:    time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
		SkippedFiles: 3,
		Series: []caseimport.SeriesOption{
			{ID: "key-1", Index: 1, Modality: "CT", InstanceCount: 158, Suitable: true},
			{ID: "key-2", Index: 2, Modality: "CT", InstanceCount: 1, Suitable: false, ReasonCode: caseimport.ReasonSingleSlice},
		},
	}
}

func TestUploadCreateReturnsRedactedSession(t *testing.T) {
	service := &fakeUploadService{prepareResult: sampleSession()}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)

	recorder := doUpload(t, router, uploadRequest(t, http.MethodPost, "/api/uploads", []uploadPart{
		{name: "slice1", data: []byte("aaa")},
		{name: "slice2", data: []byte("bbb")},
	}))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("want no-store, got %q", recorder.Header().Get("Cache-Control"))
	}
	payload := decodeBody(t, recorder)
	for _, key := range []string{"id", "expires_at", "skipped_files", "series", "selected_series_id", "case_id"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("missing allowlisted key %q in %v", key, payload)
		}
	}
	if len(payload) != 6 {
		t.Fatalf("session response leaked extra keys: %v", payload)
	}
	if payload["skipped_files"].(float64) != 3 || payload["case_id"] != nil || payload["selected_series_id"] != nil {
		t.Fatalf("unexpected session values: %v", payload)
	}
	series := payload["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("want 2 series, got %d", len(series))
	}
	first := series[0].(map[string]any)
	if len(first) != 6 {
		t.Fatalf("series option leaked extra keys: %v", first)
	}
	if first["suitable"] != true || first["reason_code"] != nil || first["modality"] != "CT" {
		t.Fatalf("unexpected first option: %v", first)
	}
	if service.prepareSkipped != 0 || len(service.prepareFiles) != 2 {
		t.Fatalf("handler must forward every staged file: %+v", service)
	}
}

func TestUploadCreateForwardsSkipCount(t *testing.T) {
	service := &fakeUploadService{prepareResult: sampleSession()}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)
	doUpload(t, router, uploadRequest(t, http.MethodPost, "/api/uploads", []uploadPart{
		{name: "DICOMDIR", data: []byte("index")},
		{name: "slice", data: []byte("aaa")},
	}))
	if service.prepareSkipped != 1 || len(service.prepareFiles) != 1 {
		t.Fatalf("want 1 skipped and 1 staged file, got %d/%d", service.prepareSkipped, len(service.prepareFiles))
	}
}

func TestUploadCreateRejectsCrossOrigin(t *testing.T) {
	service := &fakeUploadService{prepareResult: sampleSession()}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)
	request := uploadRequest(t, http.MethodPost, "/api/uploads", []uploadPart{{name: "a", data: []byte("a")}})
	request.Header.Set("Origin", "http://evil.example")

	recorder := doUpload(t, router, request)
	if recorder.Code != http.StatusForbidden || uploadErrorCode(t, decodeBody(t, recorder)) != "ORIGIN_NOT_ALLOWED" {
		t.Fatalf("want 403 ORIGIN_NOT_ALLOWED, got %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestUploadCreateRejectsUnknownFieldAndOversize(t *testing.T) {
	service := &fakeUploadService{prepareResult: sampleSession()}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)

	recorder := doUpload(t, router, uploadRequest(t, http.MethodPost, "/api/uploads", []uploadPart{{name: "a", field: "other", data: []byte("a")}}))
	if recorder.Code != http.StatusBadRequest || uploadErrorCode(t, decodeBody(t, recorder)) != "INVALID_REQUEST" {
		t.Fatalf("want 400 INVALID_REQUEST, got %d %s", recorder.Code, recorder.Body.String())
	}

	router = uploadRouter(t, service, upload.DefaultLimits(), 8)
	recorder = doUpload(t, router, uploadRequest(t, http.MethodPost, "/api/uploads", []uploadPart{{name: "big", data: bytes.Repeat([]byte("x"), 4096)}}))
	if recorder.Code != http.StatusRequestEntityTooLarge || uploadErrorCode(t, decodeBody(t, recorder)) != "UPLOAD_TOO_LARGE" {
		t.Fatalf("want 413 UPLOAD_TOO_LARGE, got %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestUploadCreateMapsUnsafeArchive(t *testing.T) {
	service := &fakeUploadService{prepareResult: sampleSession()}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)
	archive := zipBytesForTest(t, map[string][]byte{"../evil.dcm": []byte("x")}, []string{"../evil.dcm"})
	recorder := doUpload(t, router, uploadRequest(t, http.MethodPost, "/api/uploads", []uploadPart{{name: "bad.zip", data: archive}}))
	if recorder.Code != http.StatusBadRequest || uploadErrorCode(t, decodeBody(t, recorder)) != "INVALID_UPLOAD" {
		t.Fatalf("want 400 INVALID_UPLOAD, got %d %s", recorder.Code, recorder.Body.String())
	}

	truncated := zipBytesForTest(t, map[string][]byte{"a.dcm": []byte("payload")}, []string{"a.dcm"})
	recorder = doUpload(t, router, uploadRequest(t, http.MethodPost, "/api/uploads", []uploadPart{{name: "cut.zip", data: truncated[:len(truncated)-8]}}))
	if recorder.Code != http.StatusUnsupportedMediaType || uploadErrorCode(t, decodeBody(t, recorder)) != "UNSUPPORTED_UPLOAD_FORMAT" {
		t.Fatalf("want 415 UNSUPPORTED_UPLOAD_FORMAT, got %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestUploadCreateMapsSessionFailures(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{caseimport.Fail(caseimport.ErrBusy, "busy"), http.StatusTooManyRequests, "UPLOAD_BUSY"},
		{caseimport.Fail(caseimport.ErrCapacity, "full"), http.StatusTooManyRequests, "UPLOAD_CAPACITY"},
		{caseimport.Fail(caseimport.ErrInvalidInput, "bad"), http.StatusBadRequest, "INVALID_UPLOAD"},
		{caseimport.Fail(caseimport.ErrDependency, "down"), http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"},
	}
	for _, test := range cases {
		service := &fakeUploadService{prepareErr: test.err}
		router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)
		recorder := doUpload(t, router, uploadRequest(t, http.MethodPost, "/api/uploads", []uploadPart{{name: "a", data: []byte("a")}}))
		if recorder.Code != test.status || uploadErrorCode(t, decodeBody(t, recorder)) != test.code {
			t.Fatalf("%v: want %d %s, got %d %s", test.err, test.status, test.code, recorder.Code, recorder.Body.String())
		}
	}
}

func TestUploadGetAndErrors(t *testing.T) {
	present := sampleSession()
	present.SelectedSeriesID = "key-1"
	present.CaseID = 42
	service := &fakeUploadService{getResult: present}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)

	recorder := doUpload(t, router, httptest.NewRequest(http.MethodGet, "/api/uploads/session-token", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", recorder.Code)
	}
	payload := decodeBody(t, recorder)
	if payload["case_id"].(float64) != 42 || payload["selected_series_id"] != "key-1" {
		t.Fatalf("unexpected get payload: %v", payload)
	}

	service.getErr = caseimport.Fail(caseimport.ErrNotFound, "missing")
	recorder = doUpload(t, router, httptest.NewRequest(http.MethodGet, "/api/uploads/nope", nil))
	if recorder.Code != http.StatusNotFound || uploadErrorCode(t, decodeBody(t, recorder)) != "UPLOAD_NOT_FOUND" {
		t.Fatalf("want 404 UPLOAD_NOT_FOUND, got %d %s", recorder.Code, recorder.Body.String())
	}

	service.getErr = caseimport.Fail(caseimport.ErrExpired, "expired")
	recorder = doUpload(t, router, httptest.NewRequest(http.MethodGet, "/api/uploads/old", nil))
	if recorder.Code != http.StatusConflict || uploadErrorCode(t, decodeBody(t, recorder)) != "UPLOAD_EXPIRED" {
		t.Fatalf("want 409 UPLOAD_EXPIRED, got %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestUploadConfirmCreatesOrReturnsExisting(t *testing.T) {
	service := &fakeUploadService{confirmResult: caseimport.Result{CaseID: 9, InstanceCount: 158, Existing: false}}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)

	recorder := doUpload(t, router, jsonRequest(t, http.MethodPost, "/api/uploads/session-token/cases", `{"series_id":"key-1"}`))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeBody(t, recorder)
	if len(payload) != 3 || payload["case_id"].(float64) != 9 || payload["existing"] != false {
		t.Fatalf("unexpected confirm payload: %v", payload)
	}
	if service.confirmID != "session-token" || service.confirmKey != "key-1" {
		t.Fatalf("confirm not forwarded: %+v", service)
	}

	service.confirmResult = caseimport.Result{CaseID: 9, InstanceCount: 158, Existing: true}
	recorder = doUpload(t, router, jsonRequest(t, http.MethodPost, "/api/uploads/session-token/cases", `{"series_id":"key-1"}`))
	if recorder.Code != http.StatusOK || decodeBody(t, recorder)["existing"] != true {
		t.Fatalf("want 200 existing, got %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestUploadConfirmBoundaryErrors(t *testing.T) {
	service := &fakeUploadService{confirmErr: caseimport.Fail(caseimport.ErrUnsuitableSeries, "bad")}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)
	recorder := doUpload(t, router, jsonRequest(t, http.MethodPost, "/api/uploads/s/cases", `{"series_id":"k"}`))
	if recorder.Code != http.StatusUnprocessableEntity || uploadErrorCode(t, decodeBody(t, recorder)) != "UNSUITABLE_SERIES" {
		t.Fatalf("want 422 UNSUITABLE_SERIES, got %d %s", recorder.Code, recorder.Body.String())
	}

	service.confirmErr = caseimport.Fail(caseimport.ErrAlreadyConfirmed, "done")
	recorder = doUpload(t, router, jsonRequest(t, http.MethodPost, "/api/uploads/s/cases", `{"series_id":"k"}`))
	if recorder.Code != http.StatusConflict || uploadErrorCode(t, decodeBody(t, recorder)) != "UPLOAD_ALREADY_CONFIRMED" {
		t.Fatalf("want 409 UPLOAD_ALREADY_CONFIRMED, got %d %s", recorder.Code, recorder.Body.String())
	}

	service.confirmErr = caseimport.Fail(caseimport.ErrCommitOutcomeUnknown, "unknown")
	recorder = doUpload(t, router, jsonRequest(t, http.MethodPost, "/api/uploads/s/cases", `{"series_id":"k"}`))
	if recorder.Code != http.StatusServiceUnavailable || uploadErrorCode(t, decodeBody(t, recorder)) != "COMMIT_OUTCOME_UNKNOWN" {
		t.Fatalf("want 503 COMMIT_OUTCOME_UNKNOWN, got %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestUploadConfirmRejectsMalformedBodies(t *testing.T) {
	service := &fakeUploadService{confirmResult: caseimport.Result{CaseID: 1}}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)

	for _, body := range []string{`{"series_id":""}`, `{"series_id":1}`, `{"unknown":"x","series_id":"k"}`, `{}`, `not-json`, `{"series_id":"k"}{"series_id":"k"}`} {
		recorder := doUpload(t, router, jsonRequest(t, http.MethodPost, "/api/uploads/s/cases", body))
		if recorder.Code != http.StatusBadRequest || uploadErrorCode(t, decodeBody(t, recorder)) != "INVALID_REQUEST" {
			t.Fatalf("body %q: want 400 INVALID_REQUEST, got %d %s", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestUploadConfirmRejectsCrossOrigin(t *testing.T) {
	service := &fakeUploadService{confirmResult: caseimport.Result{CaseID: 1}}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)
	request := jsonRequest(t, http.MethodPost, "/api/uploads/s/cases", `{"series_id":"k"}`)
	request.Header.Set("Origin", "null")
	recorder := doUpload(t, router, request)
	if recorder.Code != http.StatusForbidden || uploadErrorCode(t, decodeBody(t, recorder)) != "ORIGIN_NOT_ALLOWED" {
		t.Fatalf("want 403 ORIGIN_NOT_ALLOWED, got %d %s", recorder.Code, recorder.Body.String())
	}
	if service.confirmKey != "" {
		t.Fatal("a cross-origin confirm must not reach the service")
	}
}

func TestUploadDelete(t *testing.T) {
	service := &fakeUploadService{}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)

	recorder := doUpload(t, router, httptest.NewRequest(http.MethodDelete, "/api/uploads/session-token", nil))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", recorder.Code)
	}

	service.discardErr = caseimport.Fail(caseimport.ErrNotFound, "missing")
	recorder = doUpload(t, router, httptest.NewRequest(http.MethodDelete, "/api/uploads/nope", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", recorder.Code)
	}
}

// TestUploadErrorsNeverLeakInternalDetail is a smoke check that no error body
// carries an AI field, a path or a raw upstream message.
func TestUploadErrorsNeverLeakInternalDetail(t *testing.T) {
	service := &fakeUploadService{prepareErr: caseimport.Fail(caseimport.ErrDependency, "orthanc refused")}
	router := uploadRouter(t, service, upload.DefaultLimits(), 1<<20)
	recorder := doUpload(t, router, uploadRequest(t, http.MethodPost, "/api/uploads", []uploadPart{{name: "a", data: []byte("a")}}))
	body := strings.ToLower(recorder.Body.String())
	if strings.Contains(body, "orthanc") || strings.Contains(body, "ai_") || strings.Contains(body, "candidate") {
		t.Fatalf("error body leaked internal detail: %s", recorder.Body.String())
	}
}

func jsonRequest(t *testing.T, method, target, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func zipBytesForTest(t *testing.T, entries map[string][]byte, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, name := range order {
		w, err := writer.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(entries[name]); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}
