package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"nodule-reader/backend/internal/application/finalread"
	"nodule-reader/backend/internal/application/initialread"
)

// stubFinalStore is a fully controllable finalread.Store for transport tests.
type stubFinalStore struct {
	ai          finalread.AI
	aiErr       error
	final       finalread.Read
	finalErr    error
	replace     finalread.Read
	replaceErr  error
	finalized   finalread.FinalizeResult
	finalizeErr error

	replaceCalls  int
	finalizeCalls int
	lastReplace   finalread.Replacement
	lastUserID    int64
}

func (s *stubFinalStore) GetAI(_ context.Context, _ int64, userID int64) (finalread.AI, error) {
	s.lastUserID = userID
	return s.ai, s.aiErr
}

func (s *stubFinalStore) GetFinal(_ context.Context, _ int64, userID int64) (finalread.Read, error) {
	s.lastUserID = userID
	return s.final, s.finalErr
}

func (s *stubFinalStore) ReplaceFinal(_ context.Context, _ int64, userID int64, value finalread.Replacement) (finalread.Read, error) {
	s.replaceCalls++
	s.lastUserID = userID
	s.lastReplace = value
	return s.replace, s.replaceErr
}

func (s *stubFinalStore) Finalize(_ context.Context, _ int64, userID int64) (finalread.FinalizeResult, error) {
	s.finalizeCalls++
	s.lastUserID = userID
	return s.finalized, s.finalizeErr
}

func doFinalRequest(store finalread.Store, method, target, contentType string, body io.Reader) *httptest.ResponseRecorder {
	handler := NewRouter(nil, nil, finalread.NewService(store), nil, http.NotFoundHandler(), newFakeAuth(), false)
	request := withTestSession(httptest.NewRequest(method, target, body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeWireKeys(t *testing.T, recorder *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &object); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return object
}

func assertWireKeys(t *testing.T, object map[string]json.RawMessage, want ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for index := range got {
		if got[index] != want[index] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

const validFinalMark = `{"p1_mm":[-102.3,45.1,-180.5],"p2_mm":[-90.2,33.0,-180.5],` +
	`"center_mm":[-96.25,39.05,-180.5],"diameter_mm":17,"origin":"reader",` +
	`"ai_candidate_id":null,"view_plane_normal":[0,0,1],"note":null}`

func TestFinalReadHTTPProjections(t *testing.T) {
	t.Run("queued carries null candidates and error", func(t *testing.T) {
		store := &stubFinalStore{ai: finalread.AI{Status: finalread.AIQueued}}
		recorder := doFinalRequest(store, http.MethodGet, "/api/cases/1/ai", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q", recorder.Header().Get("Cache-Control"))
		}
		object := decodeWireKeys(t, recorder)
		assertWireKeys(t, object, "status", "candidates", "error")
		if string(object["candidates"]) != "null" || string(object["error"]) != "null" {
			t.Fatalf("candidates/error = %s/%s", object["candidates"], object["error"])
		}
	})

	t.Run("failed carries an empty candidate list and the fixed error", func(t *testing.T) {
		store := &stubFinalStore{ai: finalread.AI{
			Status:     finalread.AIFailed,
			Candidates: []finalread.Candidate{},
			Error:      &finalread.AIError{Code: finalread.AIUnavailableCode, Message: finalread.AIUnavailableMessage},
		}}
		recorder := doFinalRequest(store, http.MethodGet, "/api/cases/1/ai", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		object := decodeWireKeys(t, recorder)
		assertWireKeys(t, object, "status", "candidates", "error")
		if string(object["candidates"]) != "[]" {
			t.Fatalf("candidates = %s, want []", object["candidates"])
		}
		var failure struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(object["error"], &failure); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if failure.Message != finalread.AIUnavailableMessage {
			t.Fatalf("message = %q", failure.Message)
		}
	})

	t.Run("succeeded carries the allowlisted groups and no error", func(t *testing.T) {
		store := &stubFinalStore{ai: finalread.AI{
			Status:     finalread.AISucceeded,
			Candidates: []finalread.Candidate{{ID: "c1", CenterMM: []float64{1, 2, 3}, SizeMM: []float64{4, 5, 6}, Score: 0.9}},
			Model:      &finalread.AIModel{Bundle: "b", BundleVersion: "v", ScoreThreshold: 0.5},
			Input:      &finalread.AIInput{NumSlices: 10, SpacingMM: []float64{1, 1, 1}, SizeVoxels: []int{2, 2, 2}},
			Timing:     &finalread.AITiming{Load: 1, Inference: 2, Total: 3},
			Resources:  &finalread.AIResources{Device: "cpu"},
		}}
		recorder := doFinalRequest(store, http.MethodGet, "/api/cases/1/ai", "", nil)
		object := decodeWireKeys(t, recorder)
		assertWireKeys(t, object, "status", "candidates", "model", "input", "timing_s", "resources")
	})

	t.Run("final read DTO shape", func(t *testing.T) {
		locked := time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC)
		reason := "vessel"
		store := &stubFinalStore{final: finalread.Read{
			ID: 7, CaseID: 3, Kind: "final", Reader: "Doctor A", LockedAt: &locked,
			Marks:              []initialread.Mark{{P1MM: []float64{1, 2, 3}, P2MM: []float64{4, 5, 6}, CenterMM: []float64{2.5, 3.5, 4.5}, DiameterMM: 2, Origin: "reader", ViewPlaneNormal: []float64{0, 0, 1}}},
			CandidateDecisions: []finalread.Decision{{CandidateID: "c1", Decision: "dismissed", DismissReason: &reason}},
		}}
		recorder := doFinalRequest(store, http.MethodGet, "/api/cases/3/reads/final", "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d", recorder.Code)
		}
		object := decodeWireKeys(t, recorder)
		assertWireKeys(t, object, "id", "case_id", "kind", "reader", "locked_at", "marks", "candidate_decisions")
		if string(object["kind"]) != `"final"` {
			t.Fatalf("kind = %s", object["kind"])
		}
		var decisions []map[string]json.RawMessage
		if err := json.Unmarshal(object["candidate_decisions"], &decisions); err != nil {
			t.Fatalf("decode decisions: %v", err)
		}
		if len(decisions) != 1 {
			t.Fatalf("decisions = %+v", decisions)
		}
		assertWireKeys(t, decisions[0], "candidate_id", "decision", "dismiss_reason")
	})
}

func TestFinalReadHTTPStatuses(t *testing.T) {
	t.Run("prelock GET and PUT are forbidden before the body is read", func(t *testing.T) {
		store := &stubFinalStore{finalErr: finalread.ErrForbidden}
		for _, test := range []struct {
			method string
			target string
			body   string
		}{
			{http.MethodGet, "/api/cases/1/reads/final", ""},
			{http.MethodPut, "/api/cases/1/reads/final", "not json at all"},
		} {
			recorder := doFinalRequest(store, test.method, test.target, "text/plain", strings.NewReader(test.body))
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("%s %s status = %d, want 403", test.method, test.target, recorder.Code)
			}
			if code := errorCode(t, recorder); code != codeInitialReadRequired {
				t.Fatalf("%s code = %q", test.method, code)
			}
		}
	})

	t.Run("finalized PUT returns 409", func(t *testing.T) {
		locked := time.Now()
		store := &stubFinalStore{final: finalread.Read{LockedAt: &locked}}
		body := `{"marks":[],"candidate_decisions":[]}`
		recorder := doFinalRequest(store, http.MethodPut, "/api/cases/1/reads/final", "application/json", strings.NewReader(body))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("status = %d", recorder.Code)
		}
		if code := errorCode(t, recorder); code != codeFinalReadLocked {
			t.Fatalf("code = %q", code)
		}
		if store.replaceCalls != 0 {
			t.Fatalf("replace called on a locked final read")
		}
	})

	t.Run("error categories map to statuses", func(t *testing.T) {
		cases := []struct {
			err    error
			status int
			code   string
		}{
			{finalread.ErrInvalid, http.StatusBadRequest, codeInvalidRequest},
			{finalread.ErrNotFound, http.StatusNotFound, codeCaseNotFound},
			{finalread.ErrLocked, http.StatusConflict, codeFinalReadLocked},
			{finalread.ErrConflict, http.StatusConflict, codeFinalReadConflict},
			{finalread.ErrCommitOutcomeUnknown, http.StatusServiceUnavailable, codeCommitUnknown},
			{finalread.ErrDependency, http.StatusServiceUnavailable, codeDependencyAvailable},
			{errors.New("boom"), http.StatusInternalServerError, codeInternalError},
		}
		for _, test := range cases {
			store := &stubFinalStore{aiErr: test.err, finalErr: test.err, finalizeErr: test.err}
			for _, target := range []struct{ method, path string }{
				{http.MethodGet, "/api/cases/1/ai"},
				{http.MethodGet, "/api/cases/1/reads/final"},
				{http.MethodPost, "/api/cases/1/finalize"},
			} {
				recorder := doFinalRequest(store, target.method, target.path, "", nil)
				if recorder.Code != test.status {
					t.Fatalf("%s %s: status = %d, want %d", target.method, target.path, recorder.Code, test.status)
				}
				if code := errorCode(t, recorder); code != test.code {
					t.Fatalf("%s %s: code = %q, want %q", target.method, target.path, code, test.code)
				}
			}
		}
	})

	t.Run("invalid and nonexistent identifiers", func(t *testing.T) {
		store := &stubFinalStore{}
		for _, target := range []struct{ method, path string }{
			{http.MethodGet, "/api/cases/abc/ai"},
			{http.MethodGet, "/api/cases/0/reads/final"},
			{http.MethodPut, "/api/cases/-1/reads/final"},
			{http.MethodPost, "/api/cases/0/finalize"},
		} {
			recorder := doFinalRequest(store, target.method, target.path, "", nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s %s status = %d, want 400", target.method, target.path, recorder.Code)
			}
		}
		notFound := &stubFinalStore{finalErr: finalread.ErrNotFound}
		recorder := doFinalRequest(notFound, http.MethodGet, "/api/cases/9/reads/final", "", nil)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", recorder.Code)
		}
	})
}

func TestFinalReadHTTPReplacementDecoding(t *testing.T) {
	valid := `{"marks":[` + validFinalMark + `],"candidate_decisions":[]}`

	t.Run("valid replacement reaches the store", func(t *testing.T) {
		store := &stubFinalStore{replace: finalread.Read{ID: 1, CaseID: 1, Kind: "final", Reader: "Doctor A"}}
		recorder := doFinalRequest(store, http.MethodPut, "/api/cases/1/reads/final", "application/json", strings.NewReader(valid))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if store.replaceCalls != 1 || len(store.lastReplace.Marks) != 1 || store.lastReplace.Marks[0].DiameterMM != 17 {
			t.Fatalf("replacement = %+v calls %d", store.lastReplace, store.replaceCalls)
		}
		if store.lastReplace.Marks[0].ViewPlaneNormal == nil {
			t.Fatalf("view normal not decoded")
		}
	})

	t.Run("optional normal may be omitted", func(t *testing.T) {
		body := `{"marks":[{"p1_mm":[-102.3,45.1,-180.5],"p2_mm":[-90.2,33.0,-180.5],` +
			`"center_mm":[-96.25,39.05,-180.5],"diameter_mm":17,"origin":"reader_after_ai"}],"candidate_decisions":[]}`
		store := &stubFinalStore{}
		recorder := doFinalRequest(store, http.MethodPut, "/api/cases/1/reads/final", "application/json", strings.NewReader(body))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if store.lastReplace.Marks[0].ViewPlaneNormal != nil {
			t.Fatalf("normal = %v, want nil", store.lastReplace.Marks[0].ViewPlaneNormal)
		}
	})

	badBodies := map[string]string{
		"missing marks list":           `{"candidate_decisions":[]}`,
		"null marks list":              `{"marks":null,"candidate_decisions":[]}`,
		"missing decisions list":       `{"marks":[]}`,
		"null decisions list":          `{"marks":[],"candidate_decisions":null}`,
		"partial triple":               `{"marks":[{"p1_mm":[1,2],"p2_mm":[3,4,5],"center_mm":[1,2,3],"diameter_mm":1,"origin":"reader"}],"candidate_decisions":[]}`,
		"null triple element":          `{"marks":[{"p1_mm":[1,null,3],"p2_mm":[3,4,5],"center_mm":[1,2,3],"diameter_mm":1,"origin":"reader"}],"candidate_decisions":[]}`,
		"zero diameter":                `{"marks":[{"p1_mm":[1,2,3],"p2_mm":[4,5,6],"center_mm":[1,2,3],"diameter_mm":0,"origin":"reader"}],"candidate_decisions":[]}`,
		"zero normal":                  `{"marks":[{"p1_mm":[1,2,3],"p2_mm":[4,5,6],"center_mm":[1,2,3],"diameter_mm":1,"origin":"reader","view_plane_normal":[0,0,0]}],"candidate_decisions":[]}`,
		"unknown mark origin":          `{"marks":[{"p1_mm":[1,2,3],"p2_mm":[4,5,6],"center_mm":[1,2,3],"diameter_mm":1,"origin":"ai"}],"candidate_decisions":[]}`,
		"accepted with reason":         `{"marks":[],"candidate_decisions":[{"candidate_id":"c1","decision":"accepted","dismiss_reason":"x"}]}`,
		"dismissed without reason":     `{"marks":[],"candidate_decisions":[{"candidate_id":"c1","decision":"dismissed"}]}`,
		"linked mark without decision": `{"marks":[{"p1_mm":[1,2,3],"p2_mm":[4,5,6],"center_mm":[1,2,3],"diameter_mm":1,"origin":"from_ai_candidate","ai_candidate_id":"c1"}],"candidate_decisions":[]}`,
		"unknown field":                `{"marks":[],"candidate_decisions":[],"extra":1}`,
		"malformed json":               `{"marks":[`,
		"trailing json":                valid + `{}`,
	}
	for name, body := range badBodies {
		t.Run(name, func(t *testing.T) {
			store := &stubFinalStore{}
			recorder := doFinalRequest(store, http.MethodPut, "/api/cases/1/reads/final", "application/json", strings.NewReader(body))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
			}
			if store.replaceCalls != 0 {
				t.Fatalf("store called for an invalid body")
			}
		})
	}

	t.Run("non-json content type", func(t *testing.T) {
		store := &stubFinalStore{}
		recorder := doFinalRequest(store, http.MethodPut, "/api/cases/1/reads/final", "text/plain", strings.NewReader(valid))
		if recorder.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415", recorder.Code)
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		store := &stubFinalStore{}
		huge := `{"marks":[{"p1_mm":[1,2,3],"p2_mm":[4,5,6],"center_mm":[1,2,3],"diameter_mm":1,"origin":"reader","note":"` +
			strings.Repeat("a", maxMarksBodyBytes) + `"}],"candidate_decisions":[]}`
		recorder := doFinalRequest(store, http.MethodPut, "/api/cases/1/reads/final", "application/json", strings.NewReader(huge))
		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", recorder.Code)
		}
	})
}

func TestFinalReadHTTPFinalize(t *testing.T) {
	t.Run("empty body succeeds", func(t *testing.T) {
		store := &stubFinalStore{finalized: finalread.FinalizeResult{
			Status: "finalized", FinalizedAt: time.Now(), AIStatus: "failed",
		}}
		recorder := doFinalRequest(store, http.MethodPost, "/api/cases/1/finalize", "", strings.NewReader(""))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		object := decodeWireKeys(t, recorder)
		assertWireKeys(t, object, "status", "finalized_at", "ai_status")
		if store.finalizeCalls != 1 {
			t.Fatalf("finalize calls = %d", store.finalizeCalls)
		}
	})

	t.Run("nonempty and chunked bodies are rejected", func(t *testing.T) {
		for name, body := range map[string]io.Reader{
			"plain": strings.NewReader("{}"),
			// A wrapper hides the concrete reader type so the request has no
			// declared length and is treated as a chunked body.
			"chunked": struct{ io.Reader }{strings.NewReader("payload")},
		} {
			store := &stubFinalStore{}
			request := withTestSession(httptest.NewRequest(http.MethodPost, "/api/cases/1/finalize", body))
			recorder := httptest.NewRecorder()
			handler := NewRouter(nil, nil, finalread.NewService(store), nil, http.NotFoundHandler(), newFakeAuth(), false)
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s status = %d, want 400", name, recorder.Code)
			}
			if store.finalizeCalls != 0 {
				t.Fatalf("%s: finalize called with a body", name)
			}
		}
	})
}

func errorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope apiErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope %q: %v", recorder.Body.String(), err)
	}
	return envelope.Error.Code
}

// TestFinalReadHandlersForwardPrincipal proves every post-lock use case receives
// the authenticated user id.
func TestFinalReadHandlersForwardPrincipal(t *testing.T) {
	const principalUserID = int64(7) // newFakeAuth's fixed principal.

	check := func(t *testing.T, store *stubFinalStore, recorder *httptest.ResponseRecorder) {
		t.Helper()
		if recorder.Code < 200 || recorder.Code >= 300 {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if store.lastUserID != principalUserID {
			t.Fatalf("user = %d, want %d", store.lastUserID, principalUserID)
		}
	}

	t.Run("get ai", func(t *testing.T) {
		store := &stubFinalStore{ai: finalread.AI{Status: finalread.AIQueued}}
		recorder := doFinalRequest(store, http.MethodGet, "/api/cases/1/ai", "", nil)
		check(t, store, recorder)
	})
	t.Run("get final", func(t *testing.T) {
		store := &stubFinalStore{final: finalread.Read{ID: 1, CaseID: 1, Kind: "final", Marks: []initialread.Mark{}, CandidateDecisions: []finalread.Decision{}}}
		recorder := doFinalRequest(store, http.MethodGet, "/api/cases/1/reads/final", "", nil)
		check(t, store, recorder)
	})
	t.Run("replace final", func(t *testing.T) {
		store := &stubFinalStore{replace: finalread.Read{ID: 1, CaseID: 1, Kind: "final", Marks: []initialread.Mark{}, CandidateDecisions: []finalread.Decision{}}}
		recorder := doFinalRequest(store, http.MethodPut, "/api/cases/1/reads/final", "application/json", strings.NewReader(`{"marks":[],"candidate_decisions":[]}`))
		check(t, store, recorder)
	})
	t.Run("finalize", func(t *testing.T) {
		store := &stubFinalStore{}
		recorder := doFinalRequest(store, http.MethodPost, "/api/cases/1/finalize", "", strings.NewReader(""))
		check(t, store, recorder)
	})
}

// TestFinalReadHandlersRequirePrincipal proves the post-lock handlers fail closed
// when mounted without the session middleware.
func TestFinalReadHandlersRequirePrincipal(t *testing.T) {
	handler := &finalReadHandler{service: finalread.NewService(&stubFinalStore{})}
	router := chi.NewRouter()
	router.Get("/api/cases/{id}/ai", handler.getAI)
	router.Get("/api/cases/{id}/reads/final", handler.getFinal)
	router.Put("/api/cases/{id}/reads/final", handler.putFinal)
	router.Post("/api/cases/{id}/finalize", handler.finalize)
	cases := []struct {
		method string
		target string
	}{
		{http.MethodGet, "/api/cases/1/ai"},
		{http.MethodGet, "/api/cases/1/reads/final"},
		{http.MethodPut, "/api/cases/1/reads/final"},
		{http.MethodPost, "/api/cases/1/finalize"},
	}
	for _, test := range cases {
		t.Run(test.method+" "+test.target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(test.method, test.target, nil))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
			if code, _ := decodeAPIError(t, recorder); code != codeAuthRequired {
				t.Fatalf("code = %q, want %s", code, codeAuthRequired)
			}
		})
	}
}

// TestFinalReadAssignedMapping keeps pre-lock ErrForbidden and ownership
// mismatch distinct, and never exposes the owner.
func TestFinalReadAssignedMapping(t *testing.T) {
	t.Run("pre-lock stays initial-read-required", func(t *testing.T) {
		store := &stubFinalStore{aiErr: finalread.ErrForbidden}
		recorder := doFinalRequest(store, http.MethodGet, "/api/cases/1/ai", "", nil)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", recorder.Code)
		}
		if code := errorCode(t, recorder); code != "INITIAL_READ_REQUIRED" {
			t.Fatalf("code = %q", code)
		}
	})
	t.Run("another owner", func(t *testing.T) {
		store := &stubFinalStore{aiErr: finalread.ErrAssignedToAnotherUser}
		recorder := doFinalRequest(store, http.MethodGet, "/api/cases/1/ai", "", nil)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", recorder.Code)
		}
		code, message := decodeAPIError(t, recorder)
		if code != "CASE_ASSIGNED_TO_ANOTHER_USER" || message != apiErrorMessages[code] {
			t.Fatalf("error = %q/%q", code, message)
		}
	})
}
