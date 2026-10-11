package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"nodule-reader/backend/internal/application/initialread"
)

// stubInitialStore implements initialread.Store for transport-contract tests.
type stubInitialStore struct {
	caseResult initialread.Case
	caseErr    error

	readResult initialread.Read
	readErr    error

	savedResult []initialread.Mark
	replaceErr  error

	submitResult initialread.SubmitResult
	submitErr    error

	caseCalls    int
	readCalls    int
	replaceCalls int
	submitCalls  int
	lastCaseID   int64
	lastMarks    []initialread.Mark
}

func (s *stubInitialStore) GetCase(_ context.Context, caseID int64) (initialread.Case, error) {
	s.caseCalls++
	s.lastCaseID = caseID
	return s.caseResult, s.caseErr
}

func (s *stubInitialStore) GetOrCreateInitial(_ context.Context, caseID int64, _ string) (initialread.Read, error) {
	s.readCalls++
	s.lastCaseID = caseID
	return s.readResult, s.readErr
}

func (s *stubInitialStore) ReplaceInitialMarks(_ context.Context, caseID int64, _ string, marks []initialread.Mark) ([]initialread.Mark, error) {
	s.replaceCalls++
	s.lastCaseID = caseID
	s.lastMarks = marks
	if s.savedResult != nil {
		return s.savedResult, s.replaceErr
	}
	return marks, s.replaceErr
}

func (s *stubInitialStore) SubmitInitial(_ context.Context, caseID int64, _ string) (initialread.SubmitResult, error) {
	s.submitCalls++
	s.lastCaseID = caseID
	return s.submitResult, s.submitErr
}

func stubCase() initialread.Case {
	return initialread.Case{
		ID:                  5,
		Source:              "LIDC-IDRI/LIDC-IDRI-0001",
		StudyInstanceUID:    "1.2.3.4.5",
		SeriesInstanceUID:   "1.2.3.4.5.6",
		FrameOfReferenceUID: "1.2.3.4.5.7",
		Status:              "awaiting_read",
		CreatedAt:           time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC),
	}
}

// measuredMark is one ordinary reader measurement used for wire round-trip
// assertions.
func measuredMark() initialread.Mark {
	return initialread.Mark{
		P1MM:            []float64{-102.3, 45.1, -180.5},
		P2MM:            []float64{-90.2, 33.0, -180.5},
		CenterMM:        []float64{-96.25, 39.05, -180.5},
		DiameterMM:      17,
		Origin:          initialread.OriginReader,
		ViewPlaneNormal: []float64{0, 0, 1},
		Note:            new("17 mm solid"),
	}
}

const measuredMarkJSON = `{"p1_mm":[-102.3,45.1,-180.5],"p2_mm":[-90.2,33.0,-180.5],` +
	`"center_mm":[-96.25,39.05,-180.5],"diameter_mm":17,"origin":"reader",` +
	`"ai_candidate_id":null,"view_plane_normal":[0,0,1],"note":"17 mm solid"}`

func doInitialRequest(store initialread.Store, method, target, contentType string, body io.Reader) *httptest.ResponseRecorder {
	handler := NewRouter(nil, initialread.NewService(store), nil, http.NotFoundHandler())
	request := httptest.NewRequest(method, target, body)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func sortedKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("not a JSON object: %v (%s)", err, raw)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func assertKeys(t *testing.T, label string, raw json.RawMessage, want ...string) {
	t.Helper()
	got := sortedKeys(t, raw)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s keys = %v, want %v", label, got, want)
	}
}

func assertNoStore(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func decodeAPIError(t *testing.T, recorder *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid error envelope %q: %v", recorder.Body.String(), err)
	}
	return envelope.Error.Code, envelope.Error.Message
}

func TestInitialReadAPICaseMetadata(t *testing.T) {
	store := &stubInitialStore{caseResult: stubCase()}
	recorder := doInitialRequest(store, http.MethodGet, "/api/cases/5", "", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	assertNoStore(t, recorder)
	assertKeys(t, "case", recorder.Body.Bytes(),
		"id", "source", "study_instance_uid", "series_instance_uid",
		"frame_of_reference_uid", "status", "created_at", "finalized_at")
	if store.readCalls != 0 {
		t.Fatalf("metadata GET created %d reads, want 0", store.readCalls)
	}
	if store.lastCaseID != 5 {
		t.Fatalf("case id = %d, want 5", store.lastCaseID)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &object); err != nil {
		t.Fatalf("case body: %v", err)
	}
	for _, forbidden := range []string{"orthanc_series_id", "ai_status", "ai_job", "candidates", "score", "model", "result"} {
		if _, present := object[forbidden]; present {
			t.Fatalf("case metadata leaked %q: %s", forbidden, recorder.Body.String())
		}
	}
}

func TestInitialReadAPIGetInitial(t *testing.T) {
	locked := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	t.Run("with marks", func(t *testing.T) {
		store := &stubInitialStore{readResult: initialread.Read{
			ID: 11, CaseID: 5, Kind: "initial", Reader: "reader", Marks: []initialread.Mark{measuredMark()},
		}}
		recorder := doInitialRequest(store, http.MethodGet, "/api/cases/5/reads/initial", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		assertNoStore(t, recorder)
		assertKeys(t, "read", recorder.Body.Bytes(), "id", "case_id", "kind", "reader", "locked_at", "marks")

		var read struct {
			Kind     string            `json:"kind"`
			Reader   string            `json:"reader"`
			LockedAt *time.Time        `json:"locked_at"`
			Marks    []json.RawMessage `json:"marks"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &read); err != nil {
			t.Fatalf("read body: %v", err)
		}
		if read.Kind != "initial" || read.Reader != "reader" || read.LockedAt != nil {
			t.Fatalf("read = %+v", read)
		}
		if len(read.Marks) != 1 {
			t.Fatalf("marks = %d, want 1", len(read.Marks))
		}
		assertKeys(t, "mark", read.Marks[0],
			"p1_mm", "p2_mm", "center_mm", "diameter_mm", "origin",
			"ai_candidate_id", "view_plane_normal", "note")
	})

	t.Run("locked read keeps its timestamp", func(t *testing.T) {
		store := &stubInitialStore{readResult: initialread.Read{
			ID: 11, CaseID: 5, Kind: "initial", Reader: "reader", LockedAt: &locked, Marks: []initialread.Mark{},
		}}
		recorder := doInitialRequest(store, http.MethodGet, "/api/cases/5/reads/initial", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), `"locked_at":"2026-10-10T09:00:00Z"`) {
			t.Fatalf("locked_at missing: %s", recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), `"marks":[]`) {
			t.Fatalf("empty marks not serialized as []: %s", recorder.Body.String())
		}
	})
}

func TestInitialReadAPIReplaceMarks(t *testing.T) {
	t.Run("valid list echoes saved marks", func(t *testing.T) {
		store := &stubInitialStore{savedResult: []initialread.Mark{measuredMark()}}
		recorder := doInitialRequest(store, http.MethodPut, "/api/cases/5/reads/initial/marks",
			"application/json; charset=utf-8", strings.NewReader(`{"marks":[`+measuredMarkJSON+`]}`))

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
		}
		assertNoStore(t, recorder)
		var marks []json.RawMessage
		if err := json.Unmarshal(recorder.Body.Bytes(), &marks); err != nil {
			t.Fatalf("PUT body is not a bare array: %v (%s)", err, recorder.Body.String())
		}
		if len(marks) != 1 {
			t.Fatalf("saved marks = %d, want 1", len(marks))
		}
		assertKeys(t, "saved mark", marks[0],
			"p1_mm", "p2_mm", "center_mm", "diameter_mm", "origin",
			"ai_candidate_id", "view_plane_normal", "note")

		if store.replaceCalls != 1 {
			t.Fatalf("replace calls = %d, want 1", store.replaceCalls)
		}
		if len(store.lastMarks) != 1 || !sameMark(store.lastMarks[0], measuredMark()) {
			t.Fatalf("forwarded mark = %+v, want signed geometry preserved", store.lastMarks)
		}
	})

	t.Run("empty list clears and serializes as []", func(t *testing.T) {
		store := &stubInitialStore{}
		recorder := doInitialRequest(store, http.MethodPut, "/api/cases/5/reads/initial/marks",
			"application/json", strings.NewReader(`{"marks":[]}`))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", recorder.Code)
		}
		if got := strings.TrimSpace(recorder.Body.String()); got != "[]" {
			t.Fatalf("body = %q, want []", got)
		}
		if store.replaceCalls != 1 || len(store.lastMarks) != 0 {
			t.Fatalf("store saw %d calls / %d marks", store.replaceCalls, len(store.lastMarks))
		}
	})
}

func sameMark(a, b initialread.Mark) bool {
	if a.DiameterMM != b.DiameterMM || a.Origin != b.Origin ||
		len(a.P1MM) != len(b.P1MM) || len(a.P2MM) != len(b.P2MM) ||
		len(a.CenterMM) != len(b.CenterMM) || len(a.ViewPlaneNormal) != len(b.ViewPlaneNormal) {
		return false
	}
	for i := range a.P1MM {
		if a.P1MM[i] != b.P1MM[i] {
			return false
		}
	}
	for i := range a.P2MM {
		if a.P2MM[i] != b.P2MM[i] {
			return false
		}
	}
	for i := range a.CenterMM {
		if a.CenterMM[i] != b.CenterMM[i] {
			return false
		}
	}
	for i := range a.ViewPlaneNormal {
		if a.ViewPlaneNormal[i] != b.ViewPlaneNormal[i] {
			return false
		}
	}
	switch {
	case a.Note == nil && b.Note == nil:
	case a.Note == nil || b.Note == nil:
		return false
	case *a.Note != *b.Note:
		return false
	}
	return a.AICandidateID == nil && b.AICandidateID == nil
}

func TestInitialReadAPIInvalidCaseIDs(t *testing.T) {
	for _, id := range []string{"0", "-1", "abc", "1.5", "99999999999999999999"} {
		t.Run(id, func(t *testing.T) {
			store := &stubInitialStore{caseResult: stubCase()}
			recorder := doInitialRequest(store, http.MethodGet, "/api/cases/"+id, "", nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", recorder.Code)
			}
			if code, _ := decodeAPIError(t, recorder); code != "INVALID_REQUEST" {
				t.Fatalf("code = %q, want INVALID_REQUEST", code)
			}
			if store.caseCalls != 0 {
				t.Fatalf("store queried for a malformed id")
			}
		})
	}
}

func TestInitialReadAPIErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		store      *stubInitialStore
		method     string
		target     string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "missing case",
			store:      &stubInitialStore{caseErr: initialread.ErrNotFound},
			method:     http.MethodGet,
			target:     "/api/cases/5",
			wantStatus: http.StatusNotFound,
			wantCode:   "CASE_NOT_FOUND",
		},
		{
			name:       "locked replace",
			store:      &stubInitialStore{replaceErr: initialread.ErrLocked},
			method:     http.MethodPut,
			target:     "/api/cases/5/reads/initial/marks",
			body:       `{"marks":[]}`,
			wantStatus: http.StatusConflict,
			wantCode:   "INITIAL_READ_LOCKED",
		},
		{
			name:       "dependency unavailable is sanitized",
			store:      &stubInitialStore{replaceErr: fmt.Errorf("%w: dsn=postgres://user:hunter2@host", initialread.ErrDependency)},
			method:     http.MethodPut,
			target:     "/api/cases/5/reads/initial/marks",
			body:       `{"marks":[]}`,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "DEPENDENCY_UNAVAILABLE",
		},
		{
			name:       "commit outcome unknown",
			store:      &stubInitialStore{replaceErr: initialread.ErrCommitOutcomeUnknown},
			method:     http.MethodPut,
			target:     "/api/cases/5/reads/initial/marks",
			body:       `{"marks":[]}`,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "COMMIT_OUTCOME_UNKNOWN",
		},
		{
			name:       "unexpected fault",
			store:      &stubInitialStore{caseErr: errors.New("panic-ish")},
			method:     http.MethodGet,
			target:     "/api/cases/5",
			wantStatus: http.StatusInternalServerError,
			wantCode:   "INTERNAL_ERROR",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contentType := ""
			var body io.Reader
			if test.body != "" {
				contentType = "application/json"
				body = strings.NewReader(test.body)
			}
			recorder := doInitialRequest(test.store, test.method, test.target, contentType, body)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
			assertNoStore(t, recorder)
			code, message := decodeAPIError(t, recorder)
			if code != test.wantCode {
				t.Fatalf("code = %q, want %q", code, test.wantCode)
			}
			for _, secret := range []string{"hunter2", "dsn=", "panic-ish", "postgres://"} {
				if strings.Contains(message, secret) {
					t.Fatalf("message %q leaked %q", message, secret)
				}
			}
		})
	}
}

func TestInitialReadAPIInvalidBodies(t *testing.T) {
	long := func(value int) string { return strings.Repeat("a", value) }

	tests := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{name: "missing marks", body: `{}`, status: 400, code: "INVALID_REQUEST"},
		{name: "null marks", body: `{"marks":null}`, status: 400, code: "INVALID_REQUEST"},
		{name: "null mark object", body: `{"marks":[null]}`, status: 400, code: "INVALID_REQUEST"},
		{name: "absent numeric fields", body: `{"marks":[{}]}`, status: 400, code: "INVALID_REQUEST"},
		{
			name:   "null vector element",
			body:   `{"marks":[{"p1_mm":[1,null,3],"p2_mm":[4,5,6],"center_mm":[2,2,4],"diameter_mm":1,"origin":"reader","view_plane_normal":[0,0,1]}]}`,
			status: 400, code: "INVALID_REQUEST",
		},
		{
			name:   "short vector",
			body:   `{"marks":[{"p1_mm":[1,2],"p2_mm":[4,5,6],"center_mm":[2,2,4],"diameter_mm":1,"origin":"reader","view_plane_normal":[0,0,1]}]}`,
			status: 400, code: "INVALID_REQUEST",
		},
		{
			name:   "long vector",
			body:   `{"marks":[{"p1_mm":[1,2,3,4],"p2_mm":[4,5,6],"center_mm":[2,2,4],"diameter_mm":1,"origin":"reader","view_plane_normal":[0,0,1]}]}`,
			status: 400, code: "INVALID_REQUEST",
		},
		{
			name:   "overflowing number",
			body:   `{"marks":[{"p1_mm":[1e400,2,3],"p2_mm":[4,5,6],"center_mm":[2,2,4],"diameter_mm":1,"origin":"reader","view_plane_normal":[0,0,1]}]}`,
			status: 400, code: "INVALID_REQUEST",
		},
		{
			name:   "string instead of number",
			body:   `{"marks":[{"p1_mm":["1",2,3],"p2_mm":[4,5,6],"center_mm":[2,2,4],"diameter_mm":1,"origin":"reader","view_plane_normal":[0,0,1]}]}`,
			status: 400, code: "INVALID_REQUEST",
		},
		{
			name:   "extra reader field",
			body:   `{"marks":[{"p1_mm":[1,2,3],"p2_mm":[4,5,6],"center_mm":[2,2,4],"diameter_mm":1,"origin":"reader","view_plane_normal":[0,0,1],"reader":"dr-x"}]}`,
			status: 400, code: "INVALID_REQUEST",
		},
		{
			name:   "extra ai field",
			body:   `{"marks":[],"ai_status":"succeeded"}`,
			status: 400, code: "INVALID_REQUEST",
		},
		{
			name:   "extra uid field",
			body:   `{"marks":[],"series_instance_uid":"1.2.3"}`,
			status: 400, code: "INVALID_REQUEST",
		},
		{
			name:   "second JSON value",
			body:   `{"marks":[]}{"marks":[]}`,
			status: 400, code: "INVALID_REQUEST",
		},
		{name: "malformed JSON", body: `{"marks":[`, status: 400, code: "INVALID_REQUEST"},
		{name: "wrong origin", body: `{"marks":[{"p1_mm":[1,2,3],"p2_mm":[4,5,6],"center_mm":[2,2,4],"diameter_mm":1,"origin":"from_ai_candidate","ai_candidate_id":"c1","view_plane_normal":[0,0,1]}]}`,
			status: 400, code: "INVALID_REQUEST"},
		{
			name:   "oversized body",
			body:   `{"marks":[{"note":"` + long(1<<20+64) + `"}]}`,
			status: 413, code: "REQUEST_TOO_LARGE",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &stubInitialStore{}
			recorder := doInitialRequest(store, http.MethodPut, "/api/cases/5/reads/initial/marks",
				"application/json", strings.NewReader(test.body))
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, test.status, recorder.Body.String())
			}
			if code, _ := decodeAPIError(t, recorder); code != test.code {
				t.Fatalf("code = %q, want %q", code, test.code)
			}
			if store.replaceCalls != 0 {
				t.Fatalf("store replaced %d times, want 0", store.replaceCalls)
			}
			assertNoStore(t, recorder)
		})
	}
}

func TestInitialReadAPIRequiresJSONContentType(t *testing.T) {
	for _, contentType := range []string{"", "text/plain", "application/xml"} {
		t.Run("content-type:"+contentType, func(t *testing.T) {
			store := &stubInitialStore{}
			recorder := doInitialRequest(store, http.MethodPut, "/api/cases/5/reads/initial/marks",
				contentType, strings.NewReader(`{"marks":[]}`))
			if recorder.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415", recorder.Code)
			}
			if code, _ := decodeAPIError(t, recorder); code != "UNSUPPORTED_MEDIA_TYPE" {
				t.Fatalf("code = %q", code)
			}
			if store.replaceCalls != 0 {
				t.Fatalf("store replaced %d times, want 0", store.replaceCalls)
			}
		})
	}
}

func TestInitialSubmitHTTP(t *testing.T) {
	locked := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	t.Run("success is allowlisted and no-store", func(t *testing.T) {
		store := &stubInitialStore{submitResult: initialread.SubmitResult{
			Status: "initial_locked", LockedAt: locked, FinalReadID: 7,
		}}
		recorder := doInitialRequest(store, http.MethodPost, "/api/cases/5/reads/initial/submit", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
		}
		assertNoStore(t, recorder)
		assertKeys(t, "submit", recorder.Body.Bytes(), "status", "locked_at", "final_read_id")

		var body struct {
			Status      string    `json:"status"`
			LockedAt    time.Time `json:"locked_at"`
			FinalReadID int64     `json:"final_read_id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Status != "initial_locked" || body.FinalReadID != 7 || !body.LockedAt.Equal(locked) {
			t.Fatalf("body = %+v", body)
		}
		for _, forbidden := range []string{"candidate", "score", "model", "ai_status", "marks", "orthanc"} {
			if strings.Contains(recorder.Body.String(), forbidden) {
				t.Fatalf("submit response leaked %q: %s", forbidden, recorder.Body.String())
			}
		}
		if store.submitCalls != 1 || store.lastCaseID != 5 {
			t.Fatalf("store calls = %d case = %d", store.submitCalls, store.lastCaseID)
		}
	})

	errors := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"missing case", initialread.ErrNotFound, http.StatusNotFound, "CASE_NOT_FOUND"},
		{"locked or finalized", initialread.ErrLocked, http.StatusConflict, "INITIAL_READ_LOCKED"},
		{"missing draft or existing final", initialread.ErrConflict, http.StatusConflict, "INITIAL_READ_CONFLICT"},
		{"conflict is not a lock", fmt.Errorf("wrap: %w", initialread.ErrConflict), http.StatusConflict, "INITIAL_READ_CONFLICT"},
		{"dependency unavailable", fmt.Errorf("%w: dsn=postgres://user:hunter2@host", initialread.ErrDependency), http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"},
		{"commit outcome unknown", initialread.ErrCommitOutcomeUnknown, http.StatusServiceUnavailable, "COMMIT_OUTCOME_UNKNOWN"},
		{"unexpected fault", errors.New("panic-ish"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, test := range errors {
		t.Run(test.name, func(t *testing.T) {
			store := &stubInitialStore{submitErr: test.err}
			recorder := doInitialRequest(store, http.MethodPost, "/api/cases/5/reads/initial/submit", "", nil)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
			assertNoStore(t, recorder)
			if code, _ := decodeAPIError(t, recorder); code != test.wantCode {
				t.Fatalf("code = %q, want %q", code, test.wantCode)
			}
			for _, secret := range []string{"hunter2", "dsn=", "panic-ish", "postgres://"} {
				if strings.Contains(recorder.Body.String(), secret) {
					t.Fatalf("response leaked %q", secret)
				}
			}
		})
	}

	t.Run("invalid case ids", func(t *testing.T) {
		for _, id := range []string{"0", "-1", "abc", "1.5", "99999999999999999999"} {
			store := &stubInitialStore{}
			recorder := doInitialRequest(store, http.MethodPost, "/api/cases/"+id+"/reads/initial/submit", "", nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("id %q status = %d, want 400", id, recorder.Code)
			}
			if code, _ := decodeAPIError(t, recorder); code != "INVALID_REQUEST" {
				t.Fatalf("id %q code = %q", id, code)
			}
			if store.submitCalls != 0 {
				t.Fatalf("id %q reached the store", id)
			}
		}
	})

	t.Run("nonempty body rejected", func(t *testing.T) {
		bodies := []struct {
			name string
			body io.Reader
		}{
			{"ordinary", strings.NewReader(`{"reader":"dr-x"}`)},
			{"chunked", io.NopCloser(strings.NewReader(`{"marks":[]}`))},
			{"whitespace", strings.NewReader(" ")},
		}
		for _, test := range bodies {
			store := &stubInitialStore{}
			recorder := doInitialRequest(store, http.MethodPost, "/api/cases/5/reads/initial/submit", "application/json", test.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s body status = %d, want 400", test.name, recorder.Code)
			}
			if code, _ := decodeAPIError(t, recorder); code != "INVALID_REQUEST" {
				t.Fatalf("%s body code = %q", test.name, code)
			}
			if store.submitCalls != 0 {
				t.Fatalf("%s body reached the store", test.name)
			}
			assertNoStore(t, recorder)
		}
	})

	t.Run("empty body with json content type is accepted", func(t *testing.T) {
		store := &stubInitialStore{submitResult: initialread.SubmitResult{
			Status: "initial_locked", LockedAt: locked, FinalReadID: 1,
		}}
		recorder := doInitialRequest(store, http.MethodPost, "/api/cases/5/reads/initial/submit", "application/json", strings.NewReader(""))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
		}
	})
}
