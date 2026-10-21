package postgres_test

import (
	"context"
	"net/http"
	"time"

	"nodule-reader/backend/internal/application/auth"
)

// integrationSessionToken is the cookie value the injected fake accepts. These
// HTTP integration tests exercise persistence behavior, not the auth service,
// so they inject a fixed principal instead of creating database sessions.
const integrationSessionToken = "integration-session-token"

// integrationAuthService returns the seeded caller as the authenticated
// principal. Tests always set both fields to a real users row so ownership checks
// run against the database rather than a global id.
type integrationAuthService struct {
	userID      int64
	displayName string
}

func (integrationAuthService) Login(context.Context, string, string, time.Time) (string, auth.Session, error) {
	return "", auth.Session{}, auth.ErrInvalidCredentials
}

func (s integrationAuthService) Authenticate(_ context.Context, rawToken string, now time.Time) (auth.Session, error) {
	if rawToken != integrationSessionToken {
		return auth.Session{}, auth.ErrInvalidCredentials
	}
	return auth.Session{
		User:      auth.User{ID: s.userID, DisplayName: s.displayName},
		ExpiresAt: now.Add(auth.SessionTTL),
	}, nil
}

func (integrationAuthService) Logout(context.Context, string) error { return nil }

// withIntegrationSession attaches the accepted fake session cookie.
func withIntegrationSession(request *http.Request) *http.Request {
	request.AddCookie(&http.Cookie{Name: "nr_session", Value: integrationSessionToken})
	return request
}
