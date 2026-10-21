package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nodule-reader/backend/internal/application/auth"
)

const testSessionToken = "test-session-token"

// fakeAuthService is the transport-level authentication seam. Contract tests
// inject fixed outcomes so no database or Argon2 work is needed.
type fakeAuthService struct {
	loginToken   string
	loginSession auth.Session
	loginErr     error
	loginCalls   int
	lastLoginPIN string
	lastLoginIP  string

	authenticateSession auth.Session
	authenticateErr     error
	authenticateCalls   int
	lastToken           string

	logoutErr   error
	logoutCalls int
	lastLogout  string
}

func (f *fakeAuthService) Login(_ context.Context, pin, clientIP string, _ time.Time) (string, auth.Session, error) {
	f.loginCalls++
	f.lastLoginPIN = pin
	f.lastLoginIP = clientIP
	return f.loginToken, f.loginSession, f.loginErr
}

func (f *fakeAuthService) Authenticate(_ context.Context, rawToken string, _ time.Time) (auth.Session, error) {
	f.authenticateCalls++
	f.lastToken = rawToken
	if f.authenticateErr != nil {
		return auth.Session{}, f.authenticateErr
	}
	if rawToken != testSessionToken {
		return auth.Session{}, auth.ErrInvalidCredentials
	}
	return f.authenticateSession, nil
}

func (f *fakeAuthService) Logout(_ context.Context, rawToken string) error {
	f.logoutCalls++
	f.lastLogout = rawToken
	return f.logoutErr
}

// newFakeAuth returns a fake that accepts testSessionToken with a fixed
// principal. Tests override the outcome fields to simulate rejection.
func newFakeAuth() *fakeAuthService {
	return &fakeAuthService{
		authenticateSession: testSession(),
		loginToken:          "raw-session-token",
		loginSession:        testSession(),
	}
}

func testSession() auth.Session {
	return auth.Session{
		User:      auth.User{ID: 7, DisplayName: "Bác sĩ An"},
		ExpiresAt: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC),
	}
}

func withTestSession(request *http.Request) *http.Request {
	request.AddCookie(&http.Cookie{Name: authCookieName, Value: testSessionToken})
	return request
}

func authOnlyRouter(service authService, secure bool) http.Handler {
	return NewRouter(nil, nil, nil, nil, http.NotFoundHandler(), service, secure)
}

func postLogin(t *testing.T, handler http.Handler, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

type authWireError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeAuthError(t *testing.T, recorder *httptest.ResponseRecorder) authWireError {
	t.Helper()
	var envelope authWireError
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope %q: %v", recorder.Body.String(), err)
	}
	return envelope
}

func assertAuthError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", recorder.Code, status, recorder.Body.String())
	}
	envelope := decodeAuthError(t, recorder)
	if envelope.Error.Code != code {
		t.Fatalf("error code = %q, want %q", envelope.Error.Code, code)
	}
	if envelope.Error.Message == "" {
		t.Fatal("error message is empty")
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func sessionCookie(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == authCookieName {
			return cookie
		}
	}
	t.Fatalf("no %s cookie in %v", authCookieName, recorder.Result().Header.Values("Set-Cookie"))
	return nil
}

func TestAuthLoginSetsStrictSessionCookieAndProjection(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run("secure="+strconv.FormatBool(secure), func(t *testing.T) {
			service := newFakeAuth()
			recorder := postLogin(t, authOnlyRouter(service, secure), `{"pin":"00001234"}`, "application/json")

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
			}
			if service.loginCalls != 1 || service.lastLoginPIN != "00001234" {
				t.Fatalf("login calls/pin = %d/%q", service.loginCalls, service.lastLoginPIN)
			}
			if service.lastLoginIP == "" {
				t.Fatal("client IP was not supplied")
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}

			var body struct {
				User struct {
					ID          int64  `json:"id"`
					DisplayName string `json:"display_name"`
				} `json:"user"`
				ExpiresAt string `json:"expires_at"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode login body %q: %v", recorder.Body.String(), err)
			}
			if body.User.ID != 7 || body.User.DisplayName != "Bác sĩ An" {
				t.Fatalf("user projection = %+v", body.User)
			}
			if body.ExpiresAt != "2026-10-21T12:00:00Z" {
				t.Fatalf("expires_at = %q", body.ExpiresAt)
			}
			var keys map[string]json.RawMessage
			if err := json.Unmarshal(recorder.Body.Bytes(), &keys); err != nil {
				t.Fatalf("decode keys: %v", err)
			}
			if len(keys) != 2 {
				t.Fatalf("login body keys = %v", keys)
			}

			cookie := sessionCookie(t, recorder)
			if cookie.Value != "raw-session-token" {
				t.Fatalf("cookie value = %q", cookie.Value)
			}
			if !cookie.HttpOnly {
				t.Error("cookie must be HttpOnly")
			}
			if cookie.SameSite != http.SameSiteStrictMode {
				t.Errorf("SameSite = %v, want Strict", cookie.SameSite)
			}
			if cookie.Path != "/" {
				t.Errorf("Path = %q, want /", cookie.Path)
			}
			if cookie.MaxAge != int(auth.SessionTTL/time.Second) {
				t.Errorf("MaxAge = %d, want %d", cookie.MaxAge, int(auth.SessionTTL/time.Second))
			}
			if cookie.Secure != secure {
				t.Errorf("Secure = %v, want %v", cookie.Secure, secure)
			}
			for _, secret := range []string{"00001234", "raw-session-token"} {
				if strings.Contains(recorder.Body.String(), secret) {
					t.Errorf("response leaked %q", secret)
				}
			}
		})
	}
}

func TestAuthLoginRejectsMalformedRequests(t *testing.T) {
	tooLarge := `{"pin":"` + strings.Repeat("1", 4096) + `"}`
	cases := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantCode    string
	}{
		{"missing content type", `{"pin":"00001234"}`, "", http.StatusUnsupportedMediaType, codeUnsupportedMedia},
		{"wrong content type", `{"pin":"00001234"}`, "text/plain", http.StatusUnsupportedMediaType, codeUnsupportedMedia},
		{"empty body", ``, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"blank object", `{}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"null pin", `{"pin":null}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"numeric pin", `{"pin":12345678}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"unknown field", `{"pin":"00001234","name":"x"}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"trailing data", `{"pin":"00001234"}{"pin":"00001234"}`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"invalid json", `{"pin":`, "application/json", http.StatusBadRequest, codeInvalidRequest},
		{"too large", tooLarge, "application/json", http.StatusRequestEntityTooLarge, codeRequestTooLarge},
		{"seven digits", `{"pin":"1234567"}`, "application/json", http.StatusUnauthorized, codeInvalidCredentials},
		{"nine digits", `{"pin":"123456789"}`, "application/json", http.StatusUnauthorized, codeInvalidCredentials},
		{"empty pin", `{"pin":""}`, "application/json", http.StatusUnauthorized, codeInvalidCredentials},
		{"letters", `{"pin":"1234abcd"}`, "application/json", http.StatusUnauthorized, codeInvalidCredentials},
		{"unicode digits", `{"pin":"١٢٣٤٥٦٧٨"}`, "application/json", http.StatusUnauthorized, codeInvalidCredentials},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service := newFakeAuth()
			recorder := postLogin(t, authOnlyRouter(service, false), testCase.body, testCase.contentType)
			assertAuthError(t, recorder, testCase.wantStatus, testCase.wantCode)
			if service.loginCalls != 0 {
				t.Fatalf("malformed PIN reached the service (%d calls)", service.loginCalls)
			}
		})
	}
}

func TestAuthLoginInvalidResponsesAreIndistinguishable(t *testing.T) {
	var bodies []string
	for _, pin := range []string{"00000001", "00000002", "99999999"} {
		service := newFakeAuth()
		service.loginErr = auth.ErrInvalidCredentials
		recorder := postLogin(t, authOnlyRouter(service, false), `{"pin":"`+pin+`"}`, "application/json")
		assertAuthError(t, recorder, http.StatusUnauthorized, codeInvalidCredentials)
		bodies = append(bodies, recorder.Body.String())
	}
	if bodies[0] != bodies[1] || bodies[1] != bodies[2] {
		t.Fatalf("invalid responses differ: %q", bodies)
	}
}

func TestAuthLoginDependencyFailureIsFixed(t *testing.T) {
	service := newFakeAuth()
	service.loginErr = auth.ErrDependency
	recorder := postLogin(t, authOnlyRouter(service, false), `{"pin":"00001234"}`, "application/json")
	assertAuthError(t, recorder, http.StatusServiceUnavailable, codeDependencyAvailable)
	if strings.Contains(recorder.Body.String(), "storage") && strings.Contains(recorder.Body.String(), "SQL") {
		t.Fatalf("response leaked adapter text: %s", recorder.Body.String())
	}
}

func TestAuthLoginThrottleCarriesIntegerRetryAfter(t *testing.T) {
	// Drive the real use case so the real *auth.ThrottledError reaches the
	// transport. Missing users use a fast stub verifier, not Argon2.
	service := auth.NewService(missingUserStore{}, stubCredentials{})
	now := time.Now()
	for range 5 {
		if _, _, err := service.Login(context.Background(), "00000000", "192.0.2.1", now); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("record failure: %v", err)
		}
	}

	recorder := postLogin(t, authOnlyRouter(service, false), `{"pin":"00000000"}`, "application/json")
	assertAuthError(t, recorder, http.StatusTooManyRequests, codeTooManyAttempts)
	raw := recorder.Header().Get("Retry-After")
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After = %q, want an integer", raw)
	}
	if seconds < 1 || seconds > 900 {
		t.Fatalf("Retry-After = %d, want between 1 and 900", seconds)
	}
	if strings.Contains(recorder.Body.String(), "00000000") {
		t.Fatal("throttle response leaked the PIN")
	}
}

// missingUserStore satisfies auth.Store through embedding; only the lookup used
// by the throttle path is implemented.
type missingUserStore struct{ auth.Store }

func (missingUserStore) FindActiveUserByPINLookup(context.Context, []byte) (auth.User, string, error) {
	return auth.User{}, "", auth.ErrUserNotFound
}

// stubCredentials avoids real Argon2 cost while keeping the service contract.
type stubCredentials struct{ auth.Credentials }

func (stubCredentials) PINLookup(pin string) []byte { return []byte(pin) }

func (stubCredentials) VerifyPIN(string, string) (bool, error) { return false, nil }

func TestAuthSessionReturnsProjectionOrAuthRequired(t *testing.T) {
	t.Run("valid cookie", func(t *testing.T) {
		service := newFakeAuth()
		request := withTestSession(httptest.NewRequest(http.MethodGet, "/api/auth/session", nil))
		recorder := httptest.NewRecorder()
		authOnlyRouter(service, false).ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if service.authenticateCalls != 1 || service.lastToken != testSessionToken {
			t.Fatalf("authenticate calls/token = %d/%q", service.authenticateCalls, service.lastToken)
		}
		want := `{"user":{"id":7,"display_name":"Bác sĩ An"},"expires_at":"2026-10-21T12:00:00Z"}`
		if got := strings.TrimSpace(recorder.Body.String()); got != want {
			t.Fatalf("body = %s, want %s", got, want)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
		if recorder.Header().Get("Set-Cookie") != "" {
			t.Fatal("session inspection must not set a cookie")
		}
	})

	cases := []struct {
		name       string
		cookie     bool
		err        error
		wantStatus int
		wantCode   string
	}{
		{"missing cookie", false, nil, http.StatusUnauthorized, codeAuthRequired},
		{"rejected cookie", true, auth.ErrInvalidCredentials, http.StatusUnauthorized, codeAuthRequired},
		{"storage failure", true, auth.ErrDependency, http.StatusServiceUnavailable, codeDependencyAvailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service := newFakeAuth()
			service.authenticateErr = testCase.err
			request := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
			if testCase.cookie {
				withTestSession(request)
			}
			recorder := httptest.NewRecorder()
			authOnlyRouter(service, false).ServeHTTP(recorder, request)
			assertAuthError(t, recorder, testCase.wantStatus, testCase.wantCode)
			if recorder.Header().Get("Set-Cookie") != "" {
				t.Fatal("failed session inspection must not set a cookie")
			}
		})
	}
}

func TestAuthLogoutClearsCookieAndIsIdempotent(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		service := newFakeAuth()
		request := withTestSession(httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil))
		recorder := httptest.NewRecorder()
		authOnlyRouter(service, false).ServeHTTP(recorder, request)

		if recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if recorder.Body.Len() != 0 {
			t.Fatalf("logout body = %q", recorder.Body.String())
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
		if service.logoutCalls != 1 || service.lastLogout != testSessionToken {
			t.Fatalf("logout calls/token = %d/%q", service.logoutCalls, service.lastLogout)
		}
		cookie := sessionCookie(t, recorder)
		if cookie.Name != authCookieName || cookie.Value != "" || cookie.MaxAge >= 0 {
			t.Fatalf("clearing cookie = %+v", cookie)
		}
		if !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteStrictMode {
			t.Fatalf("clearing cookie flags = %+v", cookie)
		}
	})

	t.Run("deletion failure still clears", func(t *testing.T) {
		service := newFakeAuth()
		service.logoutErr = auth.ErrDependency
		request := withTestSession(httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil))
		recorder := httptest.NewRecorder()
		authOnlyRouter(service, false).ServeHTTP(recorder, request)

		if recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		cookie := sessionCookie(t, recorder)
		if cookie.Value != "" || cookie.MaxAge >= 0 {
			t.Fatalf("clearing cookie = %+v", cookie)
		}
	})

	t.Run("protected without cookie", func(t *testing.T) {
		service := newFakeAuth()
		request := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
		recorder := httptest.NewRecorder()
		authOnlyRouter(service, false).ServeHTTP(recorder, request)

		assertAuthError(t, recorder, http.StatusUnauthorized, codeAuthRequired)
		if service.logoutCalls != 0 {
			t.Fatal("logout ran without a session")
		}
	})

	t.Run("non-empty body rejected", func(t *testing.T) {
		service := newFakeAuth()
		request := withTestSession(httptest.NewRequest(http.MethodPost, "/api/auth/logout", strings.NewReader(`{}`)))
		recorder := httptest.NewRecorder()
		authOnlyRouter(service, false).ServeHTTP(recorder, request)

		assertAuthError(t, recorder, http.StatusBadRequest, codeInvalidRequest)
		if service.logoutCalls != 0 {
			t.Fatal("logout ran with a body")
		}
	})
}

func TestAuthEndpointsDoNotLogSecretMaterial(t *testing.T) {
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	defer log.SetOutput(previous)

	service := newFakeAuth()
	postLogin(t, authOnlyRouter(service, false), `{"pin":"00001234"}`, "application/json")

	recorder := httptest.NewRecorder()
	authOnlyRouter(service, false).ServeHTTP(
		recorder,
		withTestSession(httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)),
	)

	logged := captured.String()
	for _, secret := range []string{"00001234", "raw-session-token", testSessionToken} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %s", secret, logged)
		}
	}
}
