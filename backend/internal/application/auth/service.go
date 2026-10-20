package auth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// SessionTTL is the absolute session lifetime. Requests never extend it.
	SessionTTL = 8 * time.Hour

	failureWindow   = 15 * time.Minute
	lockDuration    = 15 * time.Minute
	maxFailures     = 5
	maxNameRunes    = 100
	pinAttempts     = 10
	defaultMaxLimit = 4096

	// maxConcurrentVerifications bounds the number of Argon2id verifications in
	// flight per process. Argon2id is deliberately memory-hard, so an unbounded
	// login burst could exhaust memory; the semaphore queues the rest.
	maxConcurrentVerifications = 8
)

// dummyPINVerifier is a structurally valid Argon2id verifier with fixed salt and
// digest. Verifying against it costs the same as a real verification and can
// never succeed, so missing and disabled users do not stand out by timing.
var dummyPINVerifier = "$argon2id$v=19$m=65536,t=3,p=1$" +
	base64.RawURLEncoding.EncodeToString(make([]byte, 16)) + "$" +
	base64.RawURLEncoding.EncodeToString(make([]byte, 32))

// Service implements the PIN authentication use cases. Time is supplied by the
// caller and all cryptographic randomness comes from Credentials, so both are
// injectable in tests.
type Service struct {
	store       Store
	credentials Credentials
	limiter     *loginLimiter
	verifySlots chan struct{}
}

// NewService builds the use case from its persistence port and credential
// primitive.
func NewService(store Store, credentials Credentials) *Service {
	return &Service{
		store:       store,
		credentials: credentials,
		limiter:     newLoginLimiter(defaultMaxLimit),
		verifySlots: make(chan struct{}, maxConcurrentVerifications),
	}
}

// acquireVerification reserves a verification slot, waiting only as long as
// ctx allows. A canceled or expired context never starts a verification.
func (s *Service) acquireVerification(ctx context.Context) error {
	select {
	case s.verifySlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ErrDependency
	}
}

// releaseVerification returns a slot reserved by acquireVerification.
func (s *Service) releaseVerification() {
	<-s.verifySlots
}

// Login exchanges a PIN for a new opaque session token. Missing, disabled and
// wrong PINs all return ErrInvalidCredentials; key-level throttling returns a
// ThrottledError carrying the remaining lock time. Verifications are bounded by
// a per-process semaphore, so a request whose context is canceled while waiting
// returns ErrDependency without starting one.
func (s *Service) Login(ctx context.Context, pin, clientIP string, now time.Time) (string, Session, error) {
	lookup := s.credentials.PINLookup(pin)
	key := limiterKey(clientIP, lookup)
	if retryAfter, limited := s.limiter.check(key, now); limited {
		return "", Session{}, &ThrottledError{retryAfter: retryAfter}
	}

	user, encoded, err := s.store.FindActiveUserByPINLookup(ctx, lookup)
	switch {
	case errors.Is(err, ErrUserNotFound):
		// Pay the same Argon2id cost as a real verification before failing.
		encoded = dummyPINVerifier
	case err != nil:
		return "", Session{}, ErrDependency
	}

	if err := s.acquireVerification(ctx); err != nil {
		return "", Session{}, err
	}
	valid, verifyErr := s.credentials.VerifyPIN(pin, encoded)
	s.releaseVerification()

	if errors.Is(err, ErrUserNotFound) {
		s.limiter.recordFailure(key, now)
		return "", Session{}, ErrInvalidCredentials
	}
	if verifyErr != nil || !valid {
		s.limiter.recordFailure(key, now)
		return "", Session{}, ErrInvalidCredentials
	}

	raw, digest, err := s.credentials.GenerateSessionToken()
	if err != nil {
		return "", Session{}, ErrDependency
	}
	expiresAt := now.Add(SessionTTL)
	if err := s.store.CreateSession(ctx, digest, user.ID, expiresAt); err != nil {
		return "", Session{}, ErrDependency
	}
	s.limiter.clear(key)
	return raw, Session{User: user, ExpiresAt: expiresAt}, nil
}

// Authenticate resolves an opaque token to its active session. Malformed,
// expired, deleted and disabled-user sessions are indistinguishable.
func (s *Service) Authenticate(ctx context.Context, rawToken string, now time.Time) (Session, error) {
	digest, err := s.credentials.SessionDigest(rawToken)
	if err != nil {
		return Session{}, ErrInvalidCredentials
	}
	session, err := s.store.FindSession(ctx, digest, now)
	switch {
	case errors.Is(err, ErrSessionNotFound), errors.Is(err, ErrUserNotFound):
		return Session{}, ErrInvalidCredentials
	case err != nil:
		return Session{}, ErrDependency
	}
	if !session.ExpiresAt.After(now) {
		return Session{}, ErrInvalidCredentials
	}
	return session, nil
}

// Logout deletes the session behind rawToken. It is idempotent: a malformed,
// absent or already deleted session is not an error.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	digest, err := s.credentials.SessionDigest(rawToken)
	if err != nil {
		return nil
	}
	if err := s.store.DeleteSession(ctx, digest); err != nil {
		return ErrDependency
	}
	return nil
}

// CreateUser generates a PIN, stores only its lookup and verifier, and returns
// the raw PIN exactly once. A lookup collision is retried up to ten attempts.
func (s *Service) CreateUser(ctx context.Context, displayName string) (User, string, error) {
	name, err := normalizeDisplayName(displayName)
	if err != nil {
		return User{}, "", err
	}
	for range pinAttempts {
		pin, err := s.credentials.GeneratePIN()
		if err != nil {
			return User{}, "", ErrDependency
		}
		lookup := s.credentials.PINLookup(pin)
		hash, err := s.credentials.HashPIN(pin)
		if err != nil {
			return User{}, "", ErrDependency
		}
		user, err := s.store.CreateUser(ctx, name, lookup, hash)
		switch {
		case err == nil:
			return user, pin, nil
		case errors.Is(err, ErrDuplicatePINLookup):
			continue
		default:
			return User{}, "", ErrDependency
		}
	}
	return User{}, "", ErrDependency
}

// RotatePIN replaces the user's PIN and revokes every session in one store
// transaction. The raw replacement PIN is returned exactly once.
func (s *Service) RotatePIN(ctx context.Context, userID int64) (string, error) {
	if userID <= 0 {
		return "", ErrInvalidInput
	}
	for range pinAttempts {
		pin, err := s.credentials.GeneratePIN()
		if err != nil {
			return "", ErrDependency
		}
		lookup := s.credentials.PINLookup(pin)
		hash, err := s.credentials.HashPIN(pin)
		if err != nil {
			return "", ErrDependency
		}
		switch err := s.store.RotatePIN(ctx, userID, lookup, hash); {
		case err == nil:
			return pin, nil
		case errors.Is(err, ErrDuplicatePINLookup):
			continue
		case errors.Is(err, ErrUserNotFound):
			return "", ErrUserNotFound
		default:
			return "", ErrDependency
		}
	}
	return "", ErrDependency
}

// RevokeUser disables the user and deletes every session in one store
// transaction. Revoking an already disabled user is idempotent.
func (s *Service) RevokeUser(ctx context.Context, userID int64) error {
	if userID <= 0 {
		return ErrInvalidInput
	}
	switch err := s.store.RevokeUser(ctx, userID); {
	case err == nil:
		return nil
	case errors.Is(err, ErrUserNotFound):
		return ErrUserNotFound
	default:
		return ErrDependency
	}
}

// ListUsers returns every user without any credential material.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, ErrDependency
	}
	return users, nil
}

// normalizeDisplayName trims surrounding whitespace and enforces the 1-100
// character bound by Unicode code points.
func normalizeDisplayName(value string) (string, error) {
	name := strings.TrimSpace(value)
	if count := utf8.RuneCountInString(name); count < 1 || count > maxNameRunes {
		return "", ErrInvalidInput
	}
	return name, nil
}

// limiterKey binds a failure count to the client address and the peppered PIN
// lookup. It never contains a raw PIN.
func limiterKey(clientIP string, lookup []byte) string {
	return clientIP + "\x00" + hex.EncodeToString(lookup)
}

type limiterEntry struct {
	failures    int
	firstSeen   time.Time
	lastSeen    time.Time
	lockedUntil time.Time
}

// loginLimiter is a bounded, process-local failure counter. Login is
// request-facing, so every operation is serialized by mu. Locked entries are
// never extended by attempts made while locked and are evicted only after all
// unlocked entries when the map is full.
type loginLimiter struct {
	mu         sync.Mutex
	entries    map[string]*limiterEntry
	maxEntries int
}

func newLoginLimiter(maxEntries int) *loginLimiter {
	if maxEntries <= 0 {
		maxEntries = defaultMaxLimit
	}
	return &loginLimiter{entries: make(map[string]*limiterEntry), maxEntries: maxEntries}
}

// check reports whether key is locked and, if so, for how much longer.
func (l *loginLimiter) check(key string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	entry, ok := l.entries[key]
	if !ok || entry.lockedUntil.IsZero() {
		return 0, false
	}
	if entry.lockedUntil.After(now) {
		return entry.lockedUntil.Sub(now), true
	}
	delete(l.entries, key)
	return 0, false
}

// recordFailure increments the window counter and locks the key at five
// failures. The fifth failure itself reports invalid credentials.
func (l *loginLimiter) recordFailure(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	entry, ok := l.entries[key]
	if !ok {
		entry = &limiterEntry{firstSeen: now}
		l.entries[key] = entry
	}
	if now.Sub(entry.firstSeen) >= failureWindow {
		entry.failures = 0
		entry.firstSeen = now
	}
	entry.failures++
	entry.lastSeen = now
	if entry.failures >= maxFailures {
		entry.lockedUntil = now.Add(lockDuration)
	}
	l.enforceBoundLocked()
}

// clear removes the key after a successful login.
func (l *loginLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// pruneLocked drops expired locks and quiet keys. The caller holds mu.
func (l *loginLimiter) pruneLocked(now time.Time) {
	for key, entry := range l.entries {
		if !entry.lockedUntil.IsZero() {
			if !entry.lockedUntil.After(now) {
				delete(l.entries, key)
			}
			continue
		}
		if now.Sub(entry.lastSeen) >= failureWindow {
			delete(l.entries, key)
		}
	}
}

// enforceBoundLocked evicts the least recently seen unlocked entry until the
// map fits. Only when every entry is locked does it evict the oldest lock, so a
// flood of new keys cannot discard a live throttle. The caller holds mu.
func (l *loginLimiter) enforceBoundLocked() {
	for len(l.entries) > l.maxEntries {
		var oldestKey string
		var oldest time.Time
		for key, entry := range l.entries {
			if !entry.lockedUntil.IsZero() {
				continue
			}
			if oldestKey == "" || entry.lastSeen.Before(oldest) {
				oldestKey = key
				oldest = entry.lastSeen
			}
		}
		if oldestKey == "" {
			for key, entry := range l.entries {
				if oldestKey == "" || entry.lastSeen.Before(oldest) {
					oldestKey = key
					oldest = entry.lastSeen
				}
			}
		}
		delete(l.entries, oldestKey)
	}
}
