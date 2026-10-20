package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nodule-reader/backend/internal/application/auth"
	"nodule-reader/backend/internal/application/health"
)

// captureHandler records the context it received and answers with a marker.
type captureHandler struct {
	calls     int
	principal auth.Principal
	found     bool
	body      string
}

func (h *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls++
	h.principal, h.found = PrincipalFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "captured")
}

// serveSafely converts a downstream panic (an unprotected route reaching a nil
// dependency) into a test failure instead of aborting the subtests.
func serveSafely(handler http.Handler, request *http.Request) (recorder *httptest.ResponseRecorder, panicked any) {
	recorder = httptest.NewRecorder()
	func() {
		defer func() { panicked = recover() }()
		handler.ServeHTTP(recorder, request)
	}()
	return recorder, panicked
}

func TestProtectedRoutesRequireSession(t *testing.T) {
	upload := &captureHandler{}
	dicomweb := &captureHandler{}
	router := NewRouter(nil, nil, nil, upload, dicomweb, newFakeAuth(), false)

	cases := []struct {
		method string
		target string
	}{
		{http.MethodGet, "/api/cases/5"},
		{http.MethodGet, "/api/cases/5/reads/initial"},
		{http.MethodPut, "/api/cases/5/reads/initial/marks"},
		{http.MethodPost, "/api/cases/5/reads/initial/submit"},
		{http.MethodGet, "/api/cases/5/ai"},
		{http.MethodGet, "/api/cases/5/reads/final"},
		{http.MethodPut, "/api/cases/5/reads/final"},
		{http.MethodPost, "/api/cases/5/finalize"},
		{http.MethodPost, "/api/uploads"},
		{http.MethodGet, "/api/uploads/abc"},
		{http.MethodDelete, "/api/uploads/abc"},
		{http.MethodPost, "/api/uploads/abc/cases"},
		{http.MethodGet, "/dicom-web"},
		{http.MethodGet, "/dicom-web/studies"},
		{http.MethodHead, "/dicom-web/studies"},
		{http.MethodPost, "/api/auth/logout"},
	}
	for _, testCase := range cases {
		t.Run(testCase.method+" "+testCase.target, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.target, nil)
			recorder, panicked := serveSafely(router, request)
			if panicked != nil {
				t.Fatalf("request reached downstream and panicked: %v", panicked)
			}
			assertAuthError(t, recorder, http.StatusUnauthorized, codeAuthRequired)
		})
	}
	if upload.calls != 0 || dicomweb.calls != 0 {
		t.Fatalf("unauthenticated requests reached downstream (%d upload, %d dicomweb)", upload.calls, dicomweb.calls)
	}
}

func TestProtectedRoutesRejectExpiredRevokedAndFailedSessions(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"expired or revoked", auth.ErrInvalidCredentials, http.StatusUnauthorized, codeAuthRequired},
		{"storage failure", auth.ErrDependency, http.StatusServiceUnavailable, codeDependencyAvailable},
		{"unexpected failure fails closed", io.ErrUnexpectedEOF, http.StatusServiceUnavailable, codeDependencyAvailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service := newFakeAuth()
			service.authenticateErr = testCase.err
			router := NewRouter(nil, nil, nil, &captureHandler{}, &captureHandler{}, service, false)

			request := withTestSession(httptest.NewRequest(http.MethodGet, "/api/cases/5", nil))
			recorder, panicked := serveSafely(router, request)
			if panicked != nil {
				t.Fatalf("request reached downstream and panicked: %v", panicked)
			}
			assertAuthError(t, recorder, testCase.wantStatus, testCase.wantCode)
			if service.authenticateCalls != 1 {
				t.Fatalf("authenticate calls = %d", service.authenticateCalls)
			}
		})
	}
}

func TestPublicAuthAndHealthRoutesNeedNoSession(t *testing.T) {
	service := newFakeAuth()
	service.loginErr = auth.ErrInvalidCredentials
	router := NewRouter(
		health.NewService(stubProbe{}, stubProbe{}),
		nil, nil,
		&captureHandler{},
		http.NotFoundHandler(),
		service,
		false,
	)

	t.Run("health", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("health status = %d (%s)", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("login reaches handler", func(t *testing.T) {
		recorder := postLogin(t, router, `{"pin":"00001234"}`, "application/json")
		assertAuthError(t, recorder, http.StatusUnauthorized, codeInvalidCredentials)
		if service.loginCalls != 1 {
			t.Fatalf("login calls = %d", service.loginCalls)
		}
	})

	t.Run("session reaches handler", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, withTestSession(httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("session status = %d (%s)", recorder.Code, recorder.Body.String())
		}
	})
}

func TestPrincipalReachesDownstreamContext(t *testing.T) {
	upload := &captureHandler{}
	router := NewRouter(nil, nil, nil, upload, http.NotFoundHandler(), newFakeAuth(), false)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, withTestSession(httptest.NewRequest(http.MethodGet, "/api/uploads/abc", nil)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if upload.calls != 1 || !upload.found {
		t.Fatalf("downstream calls/found = %d/%v", upload.calls, upload.found)
	}
	if upload.principal.UserID != 7 || upload.principal.DisplayName != "Bác sĩ An" {
		t.Fatalf("principal = %+v", upload.principal)
	}
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Fatal("background context unexpectedly carries a principal")
	}
}

func TestUnsafeRequestsRequireSameOrigin(t *testing.T) {
	const appHost = "app.example:8443"
	service := newFakeAuth()
	service.loginToken = ""
	service.loginSession = testSession()
	upload := &captureHandler{}
	dicomweb := &captureHandler{}
	router := NewRouter(nil, nil, nil, upload, dicomweb, service, true)

	// Public login: cross-origin fails before decoding the body.
	request := httptest.NewRequest(http.MethodPost, "http://"+appHost+"/api/auth/login", strings.NewReader(`{"pin":"00001234"}`))
	request.Host = appHost
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://evil.example")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assertAuthError(t, recorder, http.StatusForbidden, codeOriginNotAllowed)
	if service.loginCalls != 0 {
		t.Fatal("cross-origin login reached the service")
	}

	// Matching public authority reaches the handler.
	request = httptest.NewRequest(http.MethodPost, "http://"+appHost+"/api/auth/login", strings.NewReader(`{"pin":"00001234"}`))
	request.Host = appHost
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://"+appHost)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || service.loginCalls != 1 {
		t.Fatalf("same-origin login status = %d, calls = %d", recorder.Code, service.loginCalls)
	}

	// Case-insensitive host comparison is allowed, as in the upload handler.
	request = httptest.NewRequest(http.MethodPost, "http://app.example:8443/api/auth/login", strings.NewReader(`{"pin":"00001234"}`))
	request.Host = "APP.EXAMPLE:8443"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://app.example:8443")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("case-insensitive login status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	// Protected unsafe routes fail before the downstream mutation.
	for _, testCase := range []struct {
		name   string
		method string
		target string
	}{
		{"upload create", http.MethodPost, "/api/uploads"},
		{"upload confirm", http.MethodPost, "/api/uploads/abc/cases"},
		{"upload discard", http.MethodDelete, "/api/uploads/abc"},
		{"logout", http.MethodPost, "/api/auth/logout"},
		{"marks", http.MethodPut, "/api/cases/1/reads/initial/marks"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, "http://"+appHost+testCase.target, nil)
			request.Host = appHost
			request.Header.Set("Origin", "http://evil.example")
			withTestSession(request)
			recorder, panicked := serveSafely(router, request)
			if panicked != nil {
				t.Fatalf("cross-origin request reached downstream: %v", panicked)
			}
			assertAuthError(t, recorder, http.StatusForbidden, codeOriginNotAllowed)
		})
	}
	if upload.calls != 0 || service.logoutCalls != 0 {
		t.Fatalf("cross-origin mutations ran (%d upload, %d logout)", upload.calls, service.logoutCalls)
	}

	// A missing Origin is a native client and remains allowed.
	request = httptest.NewRequest(http.MethodPost, "http://"+appHost+"/api/uploads", nil)
	request.Host = appHost
	withTestSession(request)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || upload.calls != 1 {
		t.Fatalf("native request status = %d, calls = %d", recorder.Code, upload.calls)
	}

	// Malformed Origins fail closed.
	for _, origin := range []string{"null", "http://", "://bad", "http://evil.example/path"} {
		request = httptest.NewRequest(http.MethodPost, "http://"+appHost+"/api/uploads", nil)
		request.Host = appHost
		request.Header.Set("Origin", origin)
		withTestSession(request)
		recorder = httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("origin %q status = %d, want 403", origin, recorder.Code)
		}
	}

	// Safe methods are exempt from the origin check.
	request = httptest.NewRequest(http.MethodGet, "http://"+appHost+"/dicom-web/studies", nil)
	request.Host = appHost
	request.Header.Set("Origin", "http://evil.example")
	withTestSession(request)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || dicomweb.calls != 1 {
		t.Fatalf("safe cross-origin GET status = %d, calls = %d", recorder.Code, dicomweb.calls)
	}
}

func TestDICOMwebReadsStreamAfterAuthentication(t *testing.T) {
	terminal := &gateTerminal{}
	router := NewRouter(nil, nil, nil, nil, NewDICOMwebHandler(terminal), newFakeAuth(), false)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			terminal.calls = 0
			request := withTestSession(httptest.NewRequest(method, "/dicom-web/studies", nil))
			request.Host = "localhost"
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
			}
			if terminal.calls != 1 {
				t.Fatalf("terminal calls = %d", terminal.calls)
			}
			if method == http.MethodGet && recorder.Body.String() != "terminal" {
				t.Fatalf("streamed body = %q", recorder.Body.String())
			}
		})
	}

	t.Run("traversal is not cleaned into an authenticated route", func(t *testing.T) {
		terminal.calls = 0
		request := withTestSession(httptest.NewRequest(http.MethodGet, "/dicom-web/studies/../series", nil))
		request.Host = "localhost"
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", recorder.Code)
		}
		if terminal.calls != 0 {
			t.Fatalf("terminal calls = %d", terminal.calls)
		}
	})
}

// Ensure the context principal accessor is the only transport accessor and is
// safe on a decorated context.
func TestPrincipalFromContextRoundTrip(t *testing.T) {
	principal := auth.Principal{UserID: 11, DisplayName: "Reader"}
	ctx := context.WithValue(context.Background(), principalContextKey{}, principal)
	got, ok := PrincipalFromContext(ctx)
	if !ok || got != principal {
		t.Fatalf("principal = %+v, ok = %v", got, ok)
	}
}
