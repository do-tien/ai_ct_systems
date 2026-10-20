// Package auth implements the PIN authentication use cases: login, session
// authentication, logout and the backend user-administration operations. It
// depends on a persistence port and a credential primitive, never on a driver.
package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sentinel error categories. The transport maps them to fixed responses; a
// category is exposed through errors.Is and never leaks adapter detail.
var (
	// ErrInvalidCredentials is returned uniformly for a wrong, missing or
	// disabled PIN and for any unusable session token.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrThrottled is returned while a (client IP, PIN lookup) key is locked.
	ErrThrottled = errors.New("too many authentication attempts")
	// ErrUserNotFound is returned only by the administration operations.
	ErrUserNotFound = errors.New("user not found")
	// ErrInvalidInput means the caller supplied malformed input.
	ErrInvalidInput = errors.New("invalid authentication input")
	// ErrDependency means the persistence dependency failed.
	ErrDependency = errors.New("authentication storage failed")
	// ErrDuplicatePINLookup is an internal adapter signal: the generated PIN
	// lookup is already taken and generation can be retried.
	ErrDuplicatePINLookup = errors.New("PIN lookup already exists")
	// ErrSessionNotFound is an internal adapter signal for an absent or
	// expired session. The service maps it to ErrInvalidCredentials.
	ErrSessionNotFound = errors.New("session not found")
)

// ThrottledError carries the remaining lock duration. RetryAfterSeconds is the
// value a transport writes into Retry-After.
type ThrottledError struct {
	retryAfter time.Duration
}

// Error never includes the client address or a PIN.
func (e *ThrottledError) Error() string {
	return fmt.Sprintf("too many authentication attempts; retry after %d seconds", e.RetryAfterSeconds())
}

// Unwrap exposes the ErrThrottled sentinel.
func (e *ThrottledError) Unwrap() error { return ErrThrottled }

// RetryAfter is the remaining lock duration.
func (e *ThrottledError) RetryAfter() time.Duration { return e.retryAfter }

// RetryAfterSeconds rounds the remaining lock up to whole seconds.
func (e *ThrottledError) RetryAfterSeconds() int {
	seconds := int((e.retryAfter + time.Second - 1) / time.Second)
	if seconds < 0 {
		return 0
	}
	return seconds
}

// User is an authentication principal as stored by the backend. DisabledAt is
// nil for an active user.
type User struct {
	ID          int64
	DisplayName string
	DisabledAt  *time.Time
}

// Principal is the authenticated identity carried in a request context.
type Principal struct {
	UserID      int64
	DisplayName string
}

// Session is one opaque authenticated session.
type Session struct {
	User      User
	ExpiresAt time.Time
}

// Credentials is the cryptographic seam: PIN lookup/verification and opaque
// session-token generation. Implementations never persist secret material.
type Credentials interface {
	GeneratePIN() (string, error)
	PINLookup(pin string) []byte
	HashPIN(pin string) (string, error)
	VerifyPIN(pin, encoded string) (bool, error)
	GenerateSessionToken() (raw string, digest []byte, err error)
	SessionDigest(raw string) ([]byte, error)
}

// Store is the persistence port. It accepts lookup digests, verifier strings,
// token digests and timestamps only; raw PINs and raw tokens never cross it.
// RotatePIN and RevokeUser must update the user and delete that user's
// sessions in one transaction.
type Store interface {
	CreateUser(ctx context.Context, displayName string, pinLookup []byte, pinHash string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	FindActiveUserByPINLookup(ctx context.Context, pinLookup []byte) (User, string, error)
	RotatePIN(ctx context.Context, userID int64, pinLookup []byte, pinHash string) error
	RevokeUser(ctx context.Context, userID int64) error
	CreateSession(ctx context.Context, tokenDigest []byte, userID int64, expiresAt time.Time) error
	FindSession(ctx context.Context, tokenDigest []byte, now time.Time) (Session, error)
	DeleteSession(ctx context.Context, tokenDigest []byte) error
	DeleteExpiredSessions(ctx context.Context, now time.Time) error
}
