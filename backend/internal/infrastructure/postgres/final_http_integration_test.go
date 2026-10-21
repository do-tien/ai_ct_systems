package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/finalread"
	"nodule-reader/backend/internal/application/initialread"
	"nodule-reader/backend/internal/infrastructure/postgres"
	httpapi "nodule-reader/backend/internal/transport/http"
)

// httpHarness wires the real router, LC1-10/LC1-13 use cases and both
// PostgreSQL adapters on a fresh disposable runtime login, and returns a
// request helper plus the runtime connection used for SQL assertions.
func httpHarness(t *testing.T) (*storageHarness, *pgxpool.Pool, func(method, target, contentType, body string) *httptest.ResponseRecorder) {
	t.Helper()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	pool := importRuntimePool(t, h)
	initialService := initialread.NewService(postgres.NewInitialReadStore(pool))
	finalService := finalread.NewService(postgres.NewFinalReadStore(pool))
	handler := httpapi.NewRouter(nil, initialService, finalService, nil, http.NotFoundHandler())
	do := func(method, target, contentType, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, target, strings.NewReader(body))
		if contentType != "" {
			request.Header.Set("Content-Type", contentType)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	return h, pool, do
}

// assertErrorEnvelope decodes the fixed {"error":{"code","message"}} envelope
// and proves nothing else accompanies it.
func assertErrorEnvelope(t *testing.T, recorder *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d (%s), want %d", recorder.Code, recorder.Body.String(), status)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v (%s)", err, recorder.Body.String())
	}
	if envelope.Error.Code != code || envelope.Error.Message != message {
		t.Fatalf("error envelope = %+v, want %s/%q", envelope.Error, code, message)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode envelope keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("error envelope carried extra members: %s", recorder.Body.String())
	}
}

// readIDOfKind returns the read id of one case and kind, failing if absent.
func readIDOfKind(t *testing.T, pool *pgxpool.Pool, caseID int64, kind string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM reads WHERE case_id = $1 AND kind = $2`, caseID, kind).Scan(&id); err != nil {
		t.Fatalf("read %s id: %v", kind, err)
	}
	return id
}

// TestFinalReadHTTPIntegration drives the real router, the LC1-10 and LC1-13 use
// cases and both PostgreSQL adapters against a real runtime login, proving the
// full browser-facing lifecycle: pre-lock 403/409 with no side effects, the
// frozen initial read after submit, post-lock AI disclosure, final replacement,
// finalize and the post-finalize 409s.
func TestFinalReadHTTPIntegration(t *testing.T) {
	h, pool, do := httpHarness(t)
	runtime := h.runtime(t)

	const seriesUID = "1.2.3.4.5.5901"
	caseID := seedCase(t, pool, seriesUID, "awaiting_read")
	seedJob(t, pool, caseID, "succeeded", syntheticAISuccess(t, seriesUID, "1.2.3.4.7", "c1"), nil)

	aiPath := fmt.Sprintf("/api/cases/%d/ai", caseID)
	finalPath := fmt.Sprintf("/api/cases/%d/reads/final", caseID)
	finalizePath := fmt.Sprintf("/api/cases/%d/finalize", caseID)
	initialPath := fmt.Sprintf("/api/cases/%d/reads/initial/marks", caseID)
	submitPath := fmt.Sprintf("/api/cases/%d/reads/initial/submit", caseID)

	// Before lock: AI is forbidden with the exact envelope and no AI content,
	// even though a succeeded job exists.
	aiRecorder := do(http.MethodGet, aiPath, "", "")
	assertErrorEnvelope(t, aiRecorder, http.StatusForbidden, "INITIAL_READ_REQUIRED", "the independent initial read is not locked")
	for _, forbidden := range []string{"candidate", "model", "score", "count", "succeeded"} {
		if strings.Contains(aiRecorder.Body.String(), forbidden) {
			t.Fatalf("pre-lock AI response leaked %q: %s", forbidden, aiRecorder.Body.String())
		}
	}
	if recorder := do(http.MethodGet, finalPath, "", ""); recorder.Code != http.StatusForbidden {
		t.Fatalf("prelock GET final status = %d", recorder.Code)
	}

	// Pre-lock finalize conflicts and leaves no durable side effect.
	stateBefore, finalizedBefore := caseStatus(t, runtime, caseID)
	finalizeRecorder := do(http.MethodPost, finalizePath, "", "")
	assertErrorEnvelope(t, finalizeRecorder, http.StatusConflict, "FINAL_READ_CONFLICT", "final read state conflicts with the request")
	stateAfter, finalizedAfter := caseStatus(t, runtime, caseID)
	if stateBefore != stateAfter || (finalizedBefore == nil) != (finalizedAfter == nil) || finalizedAfter != nil {
		t.Fatalf("pre-lock finalize changed case: %q/%v -> %q/%v", stateBefore, finalizedBefore, stateAfter, finalizedAfter)
	}
	if countEvents(t, runtime, caseID, "case_finalized") != 0 {
		t.Fatalf("pre-lock finalize recorded an event")
	}
	if countRows(t, context.Background(), pool, `SELECT count(*) FROM reads WHERE case_id = $1 AND kind = 'final'`, caseID) != 0 {
		t.Fatalf("pre-lock finalize created a final read")
	}

	// Save and submit the independent read.
	if recorder := do(http.MethodPut, initialPath, "application/json", `{"marks":[`+wireMarkJSON(t, markA())+`]}`); recorder.Code != http.StatusOK {
		t.Fatalf("PUT initial marks status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if recorder := do(http.MethodPost, submitPath, "", ""); recorder.Code != http.StatusOK {
		t.Fatalf("submit status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	// After submit the initial read is frozen: replacement and repeated submit
	// both conflict, and nothing changed.
	initialReadID := readIDOfKind(t, pool, caseID, "initial")
	initialMarks := readMarks(t, runtime, initialReadID)
	assertMarksEqual(t, initialMarks, []initialread.Mark{markA()}, "initial marks after submit")
	replaceAfterLock := do(http.MethodPut, initialPath, "application/json", `{"marks":[]}`)
	assertErrorEnvelope(t, replaceAfterLock, http.StatusConflict, "INITIAL_READ_LOCKED", "initial read is locked")
	repeatSubmit := do(http.MethodPost, submitPath, "", "")
	assertErrorEnvelope(t, repeatSubmit, http.StatusConflict, "INITIAL_READ_LOCKED", "initial read is locked")
	if got := readMarks(t, runtime, initialReadID); !reflect.DeepEqual(got, initialMarks) {
		t.Fatalf("initial marks changed after rejected mutations: %+v", got)
	}
	if readLockedAt(t, runtime, initialReadID) == nil {
		t.Fatalf("initial read lost its lock")
	}

	// The submission froze the copied final read before any replacement.
	finalID := readIDOfKind(t, pool, caseID, "final")
	assertMarksEqual(t, readMarks(t, runtime, finalID), []initialread.Mark{markA()}, "final copy after submit")

	// After lock: AI is disclosed and the copied final read is readable.
	aiAfterLock := do(http.MethodGet, aiPath, "", "")
	if aiAfterLock.Code != http.StatusOK {
		t.Fatalf("GET ai status = %d (%s)", aiAfterLock.Code, aiAfterLock.Body.String())
	}
	var aiObject map[string]json.RawMessage
	if err := json.Unmarshal(aiAfterLock.Body.Bytes(), &aiObject); err != nil {
		t.Fatalf("decode ai: %v", err)
	}
	if string(aiObject["status"]) != `"succeeded"` {
		t.Fatalf("ai status = %s", aiObject["status"])
	}
	var candidates []map[string]json.RawMessage
	if err := json.Unmarshal(aiObject["candidates"], &candidates); err != nil || len(candidates) != 1 {
		t.Fatalf("ai candidates = %s (%v)", aiObject["candidates"], err)
	}

	finalRecorder := do(http.MethodGet, finalPath, "", "")
	if finalRecorder.Code != http.StatusOK || finalRecorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET final status = %d cache = %q", finalRecorder.Code, finalRecorder.Header().Get("Cache-Control"))
	}

	// Replace the final read with a candidate-derived mark and its acceptance.
	finalMark := initialread.Mark{
		P1MM:            []float64{-102.3, 45.1, -180.5},
		P2MM:            []float64{-90.2, 33.0, -180.5},
		CenterMM:        []float64{-96.25, 39.05, -180.5},
		DiameterMM:      17,
		Origin:          finalread.OriginFromAICandidate,
		AICandidateID:   new("c1"),
		ViewPlaneNormal: []float64{0, 0, 1},
	}
	putBody := `{"marks":[` + wireMarkJSON(t, finalMark) + `],"candidate_decisions":[{"candidate_id":"c1","decision":"accepted","dismiss_reason":null}]}`
	putRecorder := do(http.MethodPut, finalPath, "application/json", putBody)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PUT final status = %d (%s)", putRecorder.Code, putRecorder.Body.String())
	}
	var savedFinal struct {
		Marks              []map[string]json.RawMessage `json:"marks"`
		CandidateDecisions []map[string]json.RawMessage `json:"candidate_decisions"`
	}
	if err := json.Unmarshal(putRecorder.Body.Bytes(), &savedFinal); err != nil {
		t.Fatalf("decode saved final: %v", err)
	}
	if len(savedFinal.Marks) != 1 || len(savedFinal.CandidateDecisions) != 1 {
		t.Fatalf("saved final = %+v", savedFinal)
	}

	// Finalize without needing a further AI event.
	finalizeAfterLock := do(http.MethodPost, finalizePath, "", "")
	if finalizeAfterLock.Code != http.StatusOK {
		t.Fatalf("finalize status = %d (%s)", finalizeAfterLock.Code, finalizeAfterLock.Body.String())
	}
	var outcome struct {
		Status   string `json:"status"`
		AIStatus string `json:"ai_status"`
	}
	if err := json.Unmarshal(finalizeAfterLock.Body.Bytes(), &outcome); err != nil {
		t.Fatalf("decode finalize: %v", err)
	}
	if outcome.Status != "finalized" || outcome.AIStatus != "succeeded" {
		t.Fatalf("finalize outcome = %+v", outcome)
	}

	// The final read remains readable and locked; every later mutation or
	// repeat conflicts.
	if recorder := do(http.MethodGet, finalPath, "", ""); recorder.Code != http.StatusOK {
		t.Fatalf("post-finalize GET final status = %d", recorder.Code)
	}
	if readLockedAt(t, runtime, finalID) == nil {
		t.Fatalf("final read was not locked by finalize")
	}
	if recorder := do(http.MethodPut, finalPath, "application/json", `{"marks":[],"candidate_decisions":[]}`); recorder.Code != http.StatusConflict {
		t.Fatalf("post-finalize PUT status = %d, want 409", recorder.Code)
	}
	if recorder := do(http.MethodPost, finalizePath, "", ""); recorder.Code != http.StatusConflict {
		t.Fatalf("repeat finalize status = %d, want 409", recorder.Code)
	}
}

// TestFinalReadHTTPPrelockCorruptResult proves the access-first precedence: a
// corrupt stored success is still invisible before the initial read is locked,
// and pre-lock finalize neither rewrites the job nor advances the case.
func TestFinalReadHTTPPrelockCorruptResult(t *testing.T) {
	h, pool, do := httpHarness(t)
	runtime := h.runtime(t)

	const seriesUID = "1.2.3.4.5.5902"
	caseID := seedCase(t, pool, seriesUID, "awaiting_read")
	jobID := seedJob(t, pool, caseID, "succeeded", []byte(`{"schema_version":"1"}`), nil)
	initialReadID := seedInitialRead(t, pool, caseID)

	clinicalBefore := snapshotClinical(t, pool)
	jobBefore := jobFingerprint(t, pool, jobID)

	aiPath := fmt.Sprintf("/api/cases/%d/ai", caseID)
	finalizePath := fmt.Sprintf("/api/cases/%d/finalize", caseID)

	aiRecorder := do(http.MethodGet, aiPath, "", "")
	assertErrorEnvelope(t, aiRecorder, http.StatusForbidden, "INITIAL_READ_REQUIRED", "the independent initial read is not locked")
	if strings.Contains(aiRecorder.Body.String(), "DEPENDENCY") || strings.Contains(aiRecorder.Body.String(), "schema_version") {
		t.Fatalf("pre-lock AI response leaked the corrupt result: %s", aiRecorder.Body.String())
	}

	finalizeRecorder := do(http.MethodPost, finalizePath, "", "")
	assertErrorEnvelope(t, finalizeRecorder, http.StatusConflict, "FINAL_READ_CONFLICT", "final read state conflicts with the request")

	if state, _ := caseStatus(t, runtime, caseID); state != "awaiting_read" {
		t.Fatalf("case advanced to %q", state)
	}
	if readLockedAt(t, runtime, initialReadID) != nil {
		t.Fatalf("initial read was locked")
	}
	if countEvents(t, runtime, caseID, "case_finalized") != 0 {
		t.Fatalf("pre-lock finalize recorded an event")
	}
	if clinicalAfter := snapshotClinical(t, pool); clinicalBefore != clinicalAfter {
		t.Fatalf("clinical state changed:\nbefore %s\nafter  %s", clinicalBefore, clinicalAfter)
	}
	if jobAfter := jobFingerprint(t, pool, jobID); jobBefore != jobAfter {
		t.Fatalf("ai_jobs row changed: before %s after %s", jobBefore, jobAfter)
	}
}

// TestFinalReadHTTPFailedJob drives the failed-AI lifecycle end to end: the AI
// status discloses only the allowlisted failure projection, the physician still
// completes with reader-only marks, and finalize records ai_status "failed".
func TestFinalReadHTTPFailedJob(t *testing.T) {
	h, pool, do := httpHarness(t)
	runtime := h.runtime(t)

	const seriesUID = "1.2.3.4.5.5903"
	caseID := seedCase(t, pool, seriesUID, "awaiting_read")
	seedJob(t, pool, caseID, "failed", nil, new("AI_TIMEOUT"))

	aiPath := fmt.Sprintf("/api/cases/%d/ai", caseID)
	finalPath := fmt.Sprintf("/api/cases/%d/reads/final", caseID)
	finalizePath := fmt.Sprintf("/api/cases/%d/finalize", caseID)
	initialPath := fmt.Sprintf("/api/cases/%d/reads/initial/marks", caseID)
	submitPath := fmt.Sprintf("/api/cases/%d/reads/initial/submit", caseID)

	if recorder := do(http.MethodPut, initialPath, "application/json", `{"marks":[`+wireMarkJSON(t, markB())+`]}`); recorder.Code != http.StatusOK {
		t.Fatalf("PUT initial marks status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if recorder := do(http.MethodPost, submitPath, "", ""); recorder.Code != http.StatusOK {
		t.Fatalf("submit status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	// The failed projection carries exactly status/candidates/error.
	aiRecorder := do(http.MethodGet, aiPath, "", "")
	if aiRecorder.Code != http.StatusOK {
		t.Fatalf("GET ai status = %d (%s)", aiRecorder.Code, aiRecorder.Body.String())
	}
	var aiObject map[string]json.RawMessage
	if err := json.Unmarshal(aiRecorder.Body.Bytes(), &aiObject); err != nil {
		t.Fatalf("decode ai: %v", err)
	}
	keys := make([]string, 0, len(aiObject))
	for key := range aiObject {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "candidates,error,status" {
		t.Fatalf("failed AI keys = %v", keys)
	}
	if string(aiObject["status"]) != `"failed"` {
		t.Fatalf("ai status = %s", aiObject["status"])
	}
	if string(aiObject["candidates"]) != `[]` {
		t.Fatalf("failed candidates = %s, want []", aiObject["candidates"])
	}
	var aiError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(aiObject["error"], &aiError); err != nil {
		t.Fatalf("decode ai error: %v", err)
	}
	if aiError.Code != "AI_TIMEOUT" || aiError.Message != finalread.AIUnavailableMessage {
		t.Fatalf("ai error = %+v", aiError)
	}

	// A physician completes with reader-only marks and no decisions.
	putBody := `{"marks":[` + wireMarkJSON(t, markB()) + `],"candidate_decisions":[]}`
	if recorder := do(http.MethodPut, finalPath, "application/json", putBody); recorder.Code != http.StatusOK {
		t.Fatalf("PUT final status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	finalizeRecorder := do(http.MethodPost, finalizePath, "", "")
	if finalizeRecorder.Code != http.StatusOK {
		t.Fatalf("finalize status = %d (%s)", finalizeRecorder.Code, finalizeRecorder.Body.String())
	}
	var outcome struct {
		Status   string `json:"status"`
		AIStatus string `json:"ai_status"`
	}
	if err := json.Unmarshal(finalizeRecorder.Body.Bytes(), &outcome); err != nil {
		t.Fatalf("decode finalize: %v", err)
	}
	if outcome.Status != "finalized" || outcome.AIStatus != "failed" {
		t.Fatalf("finalize outcome = %+v", outcome)
	}

	finalID := readIDOfKind(t, pool, caseID, "final")
	if readLockedAt(t, runtime, finalID) == nil {
		t.Fatalf("final read was not locked")
	}
	assertMarksEqual(t, readMarks(t, runtime, finalID), []initialread.Mark{markB()}, "failed-job final marks")
	events := caseFinalizedEvents(t, runtime, caseID)
	if len(events) != 1 || events[0]["ai_status"] != "failed" {
		t.Fatalf("case_finalized events = %+v", events)
	}
	lockRecorder := do(http.MethodPut, finalPath, "application/json", `{"marks":[],"candidate_decisions":[]}`)
	assertErrorEnvelope(t, lockRecorder, http.StatusConflict, "FINAL_READ_LOCKED", "the final read is locked")
}
