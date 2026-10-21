package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"nodule-reader/backend/internal/application/auth"
)

// maxLoginBodyBytes bounds the tiny login body; anything larger is rejected
// before it can reach JSON decoding.
const maxLoginBodyBytes = 1 << 10

// authHandler owns the login, session inspection and logout endpoints.
type authHandler struct {
	service      authService
	cookieSecure bool
	now          func() time.Time
}

func newAuthHandler(service authService, cookieSecure bool) *authHandler {
	return &authHandler{service: service, cookieSecure: cookieSecure, now: time.Now}
}

// loginPayload requires pin to be a JSON string. A pointer distinguishes an
// absent or null field (a malformed request) from a supplied non-string value,
// which the decoder itself rejects.
type loginPayload struct {
	PIN *string `json:"pin"`
}

// authUserResponse is the allowlisted user projection. It never carries
// credential columns or timestamps.
type authUserResponse struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"display_name"`
}

// authSessionResponse is the login/session projection.
type authSessionResponse struct {
	User      authUserResponse `json:"user"`
	ExpiresAt string           `json:"expires_at"`
}

func (h *authHandler) login(writer http.ResponseWriter, request *http.Request) {
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		writeAPIErrorStatus(writer, http.StatusUnsupportedMediaType, codeUnsupportedMedia)
		return
	}

	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxLoginBodyBytes))
	decoder.DisallowUnknownFields()

	var payload loginPayload
	if err := decoder.Decode(&payload); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIErrorStatus(writer, http.StatusRequestEntityTooLarge, codeRequestTooLarge)
			return
		}
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	if payload.PIN == nil {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}

	pin := *payload.PIN
	if !validPIN(pin) {
		writeAPIErrorStatus(writer, http.StatusUnauthorized, codeInvalidCredentials)
		return
	}

	rawToken, session, err := h.service.Login(request.Context(), pin, clientIP(request), h.now())
	if err != nil {
		writeLoginFailure(writer, err)
		return
	}
	http.SetCookie(writer, newSessionCookie(rawToken, session.ExpiresAt, h.cookieSecure))
	writeJSON(writer, http.StatusOK, sessionResponse(session))
}

// session authenticates the cookie itself because the route is public. It
// returns the same projection as a successful login.
func (h *authHandler) session(writer http.ResponseWriter, request *http.Request) {
	token, ok := sessionToken(request)
	if !ok {
		writeAPIErrorStatus(writer, http.StatusUnauthorized, codeAuthRequired)
		return
	}
	session, err := h.service.Authenticate(request.Context(), token, h.now())
	if err != nil {
		writeSessionFailure(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, sessionResponse(session))
}

// logout accepts no body, deletes the present session best-effort, always
// clears the cookie and returns 204. A repeated logout therefore stays
// idempotent even when the session row is already gone or storage fails.
func (h *authHandler) logout(writer http.ResponseWriter, request *http.Request) {
	bodyEmpty, err := requestBodyEmpty(request)
	if err != nil || !bodyEmpty {
		writeAPIErrorStatus(writer, http.StatusBadRequest, codeInvalidRequest)
		return
	}
	if token, ok := sessionToken(request); ok {
		_ = h.service.Logout(request.Context(), token)
	}
	http.SetCookie(writer, clearedSessionCookie(h.cookieSecure))
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusNoContent)
}

// validPIN requires exactly eight ASCII digits; leading zeroes are valid.
func validPIN(value string) bool {
	if len(value) != 8 {
		return false
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func sessionResponse(session auth.Session) authSessionResponse {
	return authSessionResponse{
		User: authUserResponse{
			ID:          session.User.ID,
			DisplayName: session.User.DisplayName,
		},
		ExpiresAt: session.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// newSessionCookie builds the strict browser cookie. The token is opaque and
// only ever carried in this HttpOnly, SameSite=Strict cookie.
func newSessionCookie(rawToken string, expiresAt time.Time, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     authCookieName,
		Value:    rawToken,
		Path:     "/",
		Expires:  expiresAt.UTC(),
		MaxAge:   int(auth.SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	}
}

// clearedSessionCookie expires the browser cookie in either security mode.
func clearedSessionCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	}
}

// writeLoginFailure maps login outcomes to fixed envelopes. Throttling carries
// an integer Retry-After; every credential failure is identical.
func writeLoginFailure(writer http.ResponseWriter, err error) {
	var throttled *auth.ThrottledError
	if errors.As(err, &throttled) {
		writer.Header().Set("Retry-After", strconv.Itoa(max(1, throttled.RetryAfterSeconds())))
		writeAPIErrorStatus(writer, http.StatusTooManyRequests, codeTooManyAttempts)
		return
	}
	switch {
	case errors.Is(err, auth.ErrThrottled):
		writer.Header().Set("Retry-After", "1")
		writeAPIErrorStatus(writer, http.StatusTooManyRequests, codeTooManyAttempts)
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeAPIErrorStatus(writer, http.StatusUnauthorized, codeInvalidCredentials)
	default:
		writeAPIErrorStatus(writer, http.StatusServiceUnavailable, codeDependencyAvailable)
	}
}
