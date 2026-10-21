package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nodule-reader/backend/internal/application/health"
)

type stubProbe struct {
	err error
}

func (s stubProbe) Check(context.Context) error { return s.err }

type healthBody struct {
	Status string `json:"status"`
	Checks struct {
		Backend  string `json:"backend"`
		Postgres string `json:"postgres"`
		Orthanc  string `json:"orthanc"`
	} `json:"checks"`
}

func doHealthRequest(postgresErr, orthancErr error) (int, http.Header, string) {
	handler := NewRouter(
		health.NewService(stubProbe{err: postgresErr}, stubProbe{err: orthancErr}),
		nil,                    // unused initial-read dependency for health tests
		nil,                    // unused final-read dependency for health tests
		nil,                    // unused upload dependency for health tests
		http.NotFoundHandler(), // unused DICOMweb dependency for health tests
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return recorder.Code, recorder.Header(), recorder.Body.String()
}

func TestHealthReportsOKWhenEveryDependencyIsReachable(t *testing.T) {
	code, headers, body := doHealthRequest(nil, nil)

	if code != http.StatusOK {
		t.Errorf("expected 200, got %d", code)
	}
	if got := headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("expected application/json, got %q", got)
	}
	if got := headers.Get("Cache-Control"); got != "no-store" {
		t.Errorf("expected no-store, got %q", got)
	}

	var parsed healthBody
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("invalid health JSON: %v", err)
	}
	if parsed.Status != "ok" || parsed.Checks.Backend != "ok" || parsed.Checks.Postgres != "ok" || parsed.Checks.Orthanc != "ok" {
		t.Errorf("unexpected healthy body: %s", body)
	}
}

func TestHealthReportsDegradedPerDependency(t *testing.T) {
	cases := []struct {
		name         string
		postgresErr  error
		orthancErr   error
		wantPostgres string
		wantOrthanc  string
	}{
		{"postgres down", errors.New("down"), nil, "unavailable", "ok"},
		{"orthanc down", nil, errors.New("down"), "ok", "unavailable"},
		{"both down", errors.New("down"), errors.New("down"), "unavailable", "unavailable"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			code, _, body := doHealthRequest(testCase.postgresErr, testCase.orthancErr)

			if code != http.StatusServiceUnavailable {
				t.Errorf("expected 503, got %d", code)
			}

			var parsed healthBody
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("invalid health JSON: %v", err)
			}
			if parsed.Status != "degraded" {
				t.Errorf("expected degraded, got %q", parsed.Status)
			}
			if parsed.Checks.Backend != "ok" {
				t.Errorf("expected backend ok, got %q", parsed.Checks.Backend)
			}
			if parsed.Checks.Postgres != testCase.wantPostgres {
				t.Errorf("postgres: want %q, got %q", testCase.wantPostgres, parsed.Checks.Postgres)
			}
			if parsed.Checks.Orthanc != testCase.wantOrthanc {
				t.Errorf("orthanc: want %q, got %q", testCase.wantOrthanc, parsed.Checks.Orthanc)
			}
		})
	}
}

func TestHealthDoesNotLeakProbeErrorDetails(t *testing.T) {
	secret := "postgres://nodule:super-secret-password@db:5432"
	_, _, body := doHealthRequest(errors.New(secret), errors.New(secret))

	if strings.Contains(body, "super-secret-password") {
		t.Errorf("response leaked probe error text: %s", body)
	}
}
