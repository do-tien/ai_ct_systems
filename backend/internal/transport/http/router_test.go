package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"nodule-reader/backend/internal/application/health"
)

func TestRouterRegistersPublicAndProtectedAuthSurfaces(t *testing.T) {
	service := newFakeAuth()
	router := NewRouter(
		health.NewService(stubProbe{}, stubProbe{}),
		nil, nil,
		&captureHandler{},
		NewDICOMwebHandler(&gateTerminal{}),
		service,
		false,
	)

	t.Run("health is public", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("health status = %d", recorder.Code)
		}
	})

	t.Run("login is public", func(t *testing.T) {
		recorder := postLogin(t, router, `{"pin":"00000000"}`, "application/json")
		if recorder.Code != http.StatusOK {
			t.Fatalf("login status = %d (%s)", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("session is public but authenticates its cookie", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/auth/session", nil))
		assertAuthError(t, recorder, http.StatusUnauthorized, codeAuthRequired)
	})

	protected := []struct {
		method string
		target string
	}{
		{http.MethodPost, "/api/auth/logout"},
		{http.MethodGet, "/api/cases/1"},
		{http.MethodGet, "/api/cases/1/reads/initial"},
		{http.MethodGet, "/api/cases/1/reads/final"},
		{http.MethodPost, "/api/uploads"},
		{http.MethodGet, "/dicom-web"},
		{http.MethodGet, "/dicom-web/studies"},
	}
	for _, testCase := range protected {
		t.Run("protected "+testCase.method+" "+testCase.target, func(t *testing.T) {
			recorder, panicked := serveSafely(router, httptest.NewRequest(testCase.method, testCase.target, nil))
			if panicked != nil {
				t.Fatalf("protected route reached downstream: %v", panicked)
			}
			assertAuthError(t, recorder, http.StatusUnauthorized, codeAuthRequired)
		})
	}
}

func TestRouterDICOMwebStreamsOnlyAfterAuthentication(t *testing.T) {
	terminal := &gateTerminal{}
	router := NewRouter(nil, nil, nil, nil, NewDICOMwebHandler(terminal), newFakeAuth(), false)

	request := withTestSession(httptest.NewRequest(http.MethodGet, "/dicom-web/studies", nil))
	request.Host = "localhost"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	body, _ := io.ReadAll(recorder.Result().Body)
	if string(body) != "terminal" || terminal.calls != 1 {
		t.Fatalf("body = %q, calls = %d", body, terminal.calls)
	}
}
