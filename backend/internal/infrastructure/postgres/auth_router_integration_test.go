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

type integrationAuthService struct{}

func (integrationAuthService) Login(context.Context, string, string, time.Time) (string, auth.Session, error) {
	return "", auth.Session{}, auth.ErrInvalidCredentials
}

func (integrationAuthService) Authenticate(_ context.Context, rawToken string, now time.Time) (auth.Session, error) {
	if rawToken != integrationSessionToken {
		return auth.Session{}, auth.ErrInvalidCredentials
	}
	return auth.Session{
		User:      auth.User{ID: 1, DisplayName: "Integration reader"},
		ExpiresAt: now.Add(auth.SessionTTL),
	}, nil
}

func (integrationAuthService) Logout(context.Context, string) error { return nil }

// withIntegrationSession attaches the accepted fake session cookie.
func withIntegrationSession(request *http.Request) *http.Request {
	request.AddCookie(&http.Cookie{Name: "nr_session", Value: integrationSessionToken})
	return request
}
