package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"nodule-reader/backend/internal/application/auth"
)

// authCookieName is the only browser session credential. Its value is the
// opaque token; the server stores only its SHA-256 digest.
const authCookieName = "nr_session"

// principalContextKey is private so callers can only read the principal through
// PrincipalFromContext.
type principalContextKey struct{}

// PrincipalFromContext returns the authenticated principal the session
// middleware attached to the request. It is the single transport accessor for
// downstream handlers.
func PrincipalFromContext(ctx context.Context) (auth.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(auth.Principal)
	return principal, ok
}

// requirePrincipal is the defense-in-depth check every clinical handler runs
// before touching a use case. The session middleware already rejects missing
// sessions; this makes a handler mounted without that middleware fail closed.
func requirePrincipal(writer http.ResponseWriter, request *http.Request) (auth.Principal, bool) {
	principal, ok := PrincipalFromContext(request.Context())
	if !ok {
		writeAPIErrorStatus(writer, http.StatusUnauthorized, codeAuthRequired)
		return auth.Principal{}, false
	}
	return principal, true
}

// authService is the transport-owned minimal authentication dependency. The
// composition root injects *auth.Service; tests inject a fake without creating
// database sessions.
type authService interface {
	Login(ctx context.Context, pin, clientIP string, now time.Time) (string, auth.Session, error)
	Authenticate(ctx context.Context, rawToken string, now time.Time) (auth.Session, error)
	Logout(ctx context.Context, rawToken string) error
}

// sessionMiddleware authenticates the session cookie before a protected handler
// runs and stores only the principal in the request context.
type sessionMiddleware struct {
	service authService
	now     func() time.Time
}

func newSessionMiddleware(service authService) *sessionMiddleware {
	return &sessionMiddleware{service: service, now: time.Now}
}

// gate protects by path, not only by registered route: every /api and
// /dicom-web request must authenticate, including unknown paths and methods
// that later produce 404/405. Health, login and session inspection are the only
// public requests; everything else passes through unchanged.
func (m *sessionMiddleware) gate(next http.Handler) http.Handler {
	authenticated := m.authenticate(next)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isPublicRequest(request) || !requiresSession(request.URL.Path) {
			next.ServeHTTP(writer, request)
			return
		}
		authenticated.ServeHTTP(writer, request)
	})
}

// authenticate resolves the session cookie and attaches the principal. It runs
// exactly once per protected request.
func (m *sessionMiddleware) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token, ok := sessionToken(request)
		if !ok {
			writeAPIErrorStatus(writer, http.StatusUnauthorized, codeAuthRequired)
			return
		}
		session, err := m.service.Authenticate(request.Context(), token, m.now())
		if err != nil {
			writeSessionFailure(writer, err)
			return
		}
		principal := auth.Principal{UserID: session.User.ID, DisplayName: session.User.DisplayName}
		ctx := context.WithValue(request.Context(), principalContextKey{}, principal)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

// isPublicRequest lists the exact public method/path pairs. A different method
// on the same path is protected when the path is under /api.
func isPublicRequest(request *http.Request) bool {
	switch request.URL.Path {
	case "/healthz":
		return request.Method == http.MethodGet
	case "/api/auth/login":
		return request.Method == http.MethodPost
	case "/api/auth/session":
		return request.Method == http.MethodGet
	default:
		return false
	}
}

// requiresSession reports whether a path belongs to the authenticated API or
// DICOMweb surfaces. The raw path is inspected; no cleaning happens here.
func requiresSession(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/") ||
		path == "/dicom-web" || strings.HasPrefix(path, "/dicom-web/")
}

// sessionToken extracts a present, non-empty session cookie.
func sessionToken(request *http.Request) (string, bool) {
	cookie, err := request.Cookie(authCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}

// writeSessionFailure maps authentication storage failures to a fixed 503 and
// every credential failure to a fixed 401, so a missing, expired, revoked or
// deleted session is indistinguishable. Unexpected failures fail closed.
func writeSessionFailure(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeAPIErrorStatus(writer, http.StatusUnauthorized, codeAuthRequired)
	default:
		writeAPIErrorStatus(writer, http.StatusServiceUnavailable, codeDependencyAvailable)
	}
}

// sameOriginGuard rejects unsafe browser requests whose Origin host does not
// match the preserved public Host. A request with no Origin is a native client
// and is allowed; a malformed Origin fails closed.
func sameOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isUnsafeMethod(request.Method) && !sameOrigin(request) {
			writeAPIErrorStatus(writer, http.StatusForbidden, codeOriginNotAllowed)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func sameOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, request.Host)
}

// clientIP extracts the transport peer address for login throttling. It does
// not trust a client-supplied forwarding header.
func clientIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}
