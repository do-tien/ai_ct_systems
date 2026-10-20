package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// fakeCredentials implements the Credentials seam without any real
// cryptography. Tests only assert the service's decisions.
type fakeCredentials struct {
	verify      func(pin, encoded string) (bool, error)
	verifyCalls int
	hash        func(pin string) (string, error)
	generatePIN func() (string, error)
	token       string
	digest      []byte
	digestErr   error
}

func newFakeCredentials(goodPIN, hash string) *fakeCredentials {
	return &fakeCredentials{
		verify: func(pin, encoded string) (bool, error) {
			return pin == goodPIN && encoded == hash, nil
		},
		hash: func(pin string) (string, error) { return "hash:" + pin, nil },
		generatePIN: func() (string, error) {
			return goodPIN, nil
		},
		token:  "session-token",
		digest: []byte("digest:session-token"),
	}
}

func (f *fakeCredentials) GeneratePIN() (string, error) {
	if f.generatePIN == nil {
		return "", errors.New("no PIN sequence")
	}
	return f.generatePIN()
}

func (f *fakeCredentials) PINLookup(pin string) []byte {
	return []byte("lookup:" + pin)
}

func (f *fakeCredentials) HashPIN(pin string) (string, error) { return f.hash(pin) }

func (f *fakeCredentials) VerifyPIN(pin, encoded string) (bool, error) {
	f.verifyCalls++
	return f.verify(pin, encoded)
}

func (f *fakeCredentials) GenerateSessionToken() (string, []byte, error) {
	return f.token, f.digest, nil
}

func (f *fakeCredentials) SessionDigest(raw string) ([]byte, error) {
	if f.digestErr != nil {
		return nil, f.digestErr
	}
	if raw == "" || raw == "malformed" {
		return nil, errors.New("malformed token")
	}
	return []byte("digest:" + raw), nil
}

// fakeStore is a minimal in-memory Store. It mirrors the adapter's contract:
// disabled users disappear from lookup, rotate and revoke delete sessions, and
// expired sessions are not returned.
type fakeStore struct {
	users       map[int64]User
	hashes      map[int64]string
	lookups     map[string]int64
	sessions    map[string]Session
	nextID      int64
	createCalls int
	rotateCalls int
	revokeCalls int
	deleteCalls int
	expireCalls int

	duplicateCreates int
	duplicateRotates int
	forceCreateErr   error
	forceLookupErr   error
	forceRotateErr   error
	forceRevokeErr   error
	forceSessionErr  error
	// ignoreExpiry makes FindSession return an expired row so the service's
	// own expiry guard is exercised.
	ignoreExpiry bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:    make(map[int64]User),
		hashes:   make(map[int64]string),
		lookups:  make(map[string]int64),
		sessions: make(map[string]Session),
	}
}

func (s *fakeStore) seed(id int64, name, pin, hash string, credentials Credentials) {
	s.users[id] = User{ID: id, DisplayName: name}
	s.hashes[id] = hash
	s.lookups[string(credentials.PINLookup(pin))] = id
	if id > s.nextID {
		s.nextID = id
	}
}

func (s *fakeStore) removeLookups(userID int64) {
	for key, id := range s.lookups {
		if id == userID {
			delete(s.lookups, key)
		}
	}
}

func (s *fakeStore) removeSessions(userID int64) {
	for digest, session := range s.sessions {
		if session.User.ID == userID {
			delete(s.sessions, digest)
		}
	}
}

func (s *fakeStore) CreateUser(_ context.Context, name string, lookup []byte, hash string) (User, error) {
	s.createCalls++
	if s.forceCreateErr != nil {
		return User{}, s.forceCreateErr
	}
	if s.duplicateCreates > 0 {
		s.duplicateCreates--
		return User{}, ErrDuplicatePINLookup
	}
	s.nextID++
	user := User{ID: s.nextID, DisplayName: name}
	s.users[user.ID] = user
	s.hashes[user.ID] = hash
	s.lookups[string(lookup)] = user.ID
	return user, nil
}

func (s *fakeStore) ListUsers(context.Context) ([]User, error) {
	users := make([]User, 0, len(s.users))
	for id := int64(1); id <= s.nextID; id++ {
		if user, ok := s.users[id]; ok {
			users = append(users, user)
		}
	}
	return users, nil
}

func (s *fakeStore) FindActiveUserByPINLookup(_ context.Context, lookup []byte) (User, string, error) {
	if s.forceLookupErr != nil {
		return User{}, "", s.forceLookupErr
	}
	id, ok := s.lookups[string(lookup)]
	if !ok {
		return User{}, "", ErrUserNotFound
	}
	return s.users[id], s.hashes[id], nil
}

func (s *fakeStore) RotatePIN(_ context.Context, userID int64, lookup []byte, hash string) error {
	s.rotateCalls++
	if s.forceRotateErr != nil {
		return s.forceRotateErr
	}
	if s.duplicateRotates > 0 {
		s.duplicateRotates--
		return ErrDuplicatePINLookup
	}
	if _, ok := s.users[userID]; !ok {
		return ErrUserNotFound
	}
	s.removeLookups(userID)
	s.lookups[string(lookup)] = userID
	s.hashes[userID] = hash
	s.removeSessions(userID)
	return nil
}

func (s *fakeStore) RevokeUser(_ context.Context, userID int64) error {
	s.revokeCalls++
	if s.forceRevokeErr != nil {
		return s.forceRevokeErr
	}
	user, ok := s.users[userID]
	if !ok {
		return ErrUserNotFound
	}
	now := testNow
	user.DisabledAt = &now
	s.users[userID] = user
	s.removeLookups(userID)
	s.removeSessions(userID)
	return nil
}

func (s *fakeStore) CreateSession(_ context.Context, digest []byte, userID int64, expiresAt time.Time) error {
	s.sessions[string(digest)] = Session{User: s.users[userID], ExpiresAt: expiresAt}
	return nil
}

func (s *fakeStore) FindSession(_ context.Context, digest []byte, now time.Time) (Session, error) {
	if s.forceSessionErr != nil {
		return Session{}, s.forceSessionErr
	}
	session, ok := s.sessions[string(digest)]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	if !s.ignoreExpiry && !session.ExpiresAt.After(now) {
		return Session{}, ErrSessionNotFound
	}
	return session, nil
}

func (s *fakeStore) DeleteSession(_ context.Context, digest []byte) error {
	s.deleteCalls++
	delete(s.sessions, string(digest))
	return nil
}

func (s *fakeStore) DeleteExpiredSessions(_ context.Context, _ time.Time) error {
	s.expireCalls++
	return nil
}

func TestLoginCreatesEightHourSession(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	service := NewService(store, credentials)

	raw, session, err := service.Login(context.Background(), "12345678", "10.0.0.1", testNow)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if raw != "session-token" {
		t.Fatalf("raw token mismatch")
	}
	if session.User.ID != 1 || session.User.DisplayName != "Alice" {
		t.Fatalf("unexpected session user")
	}
	if !session.ExpiresAt.Equal(testNow.Add(8 * time.Hour)) {
		t.Fatalf("expiry = %v, want eight hours", session.ExpiresAt)
	}
	stored, ok := store.sessions[string(credentials.digest)]
	if !ok {
		t.Fatal("session digest was not stored")
	}
	if !stored.ExpiresAt.Equal(testNow.Add(8 * time.Hour)) {
		t.Fatal("stored expiry is not eight hours")
	}
}

func TestLoginFailuresAreUniform(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	service := NewService(store, credentials)

	t.Run("wrong PIN", func(t *testing.T) {
		_, _, err := service.Login(context.Background(), "00000000", "10.0.0.1", testNow)
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("error = %v, want invalid credentials", err)
		}
	})

	t.Run("missing PIN", func(t *testing.T) {
		before := credentials.verifyCalls
		_, _, err := service.Login(context.Background(), "87654321", "10.0.0.2", testNow)
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("error = %v, want invalid credentials", err)
		}
		if credentials.verifyCalls == before {
			t.Fatal("a missing user skipped the dummy verification")
		}
	})

	t.Run("disabled user", func(t *testing.T) {
		if err := store.RevokeUser(context.Background(), 1); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		before := credentials.verifyCalls
		_, _, err := service.Login(context.Background(), "12345678", "10.0.0.3", testNow)
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("error = %v, want invalid credentials", err)
		}
		if credentials.verifyCalls == before {
			t.Fatal("a disabled user skipped the dummy verification")
		}
	})
}

func TestLoginThrottlesAfterFiveFailures(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	service := NewService(store, credentials)

	for attempt := range 5 {
		_, _, err := service.Login(context.Background(), "00000000", "10.0.0.1", testNow)
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d error = %v, want invalid credentials", attempt+1, err)
		}
	}

	_, _, err := service.Login(context.Background(), "00000000", "10.0.0.1", testNow)
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("error = %v, want throttled", err)
	}
	var throttled *ThrottledError
	if !errors.As(err, &throttled) {
		t.Fatalf("error does not carry retry-after information")
	}
	if throttled.RetryAfter() != 15*time.Minute {
		t.Fatalf("retry after = %v, want 15m", throttled.RetryAfter())
	}
	if throttled.RetryAfterSeconds() != 900 {
		t.Fatalf("retry-after seconds = %d, want 900", throttled.RetryAfterSeconds())
	}

	// Another client address is not throttled.
	if _, _, err := service.Login(context.Background(), "00000000", "10.0.0.2", testNow); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("a different client was throttled: %v", err)
	}
}

func TestLoginThrottleLockExpires(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	service := NewService(store, credentials)

	for range 5 {
		if _, _, err := service.Login(context.Background(), "00000000", "10.0.0.1", testNow); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure error = %v", err)
		}
	}
	if _, _, err := service.Login(context.Background(), "00000000", "10.0.0.1", testNow.Add(14*time.Minute)); !errors.Is(err, ErrThrottled) {
		t.Fatalf("attempt before lock expiry was not throttled: %v", err)
	}
	if _, _, err := service.Login(context.Background(), "00000000", "10.0.0.1", testNow.Add(15*time.Minute)); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("attempt after lock expiry error = %v, want invalid credentials", err)
	}
	if _, _, err := service.Login(context.Background(), "12345678", "10.0.0.1", testNow.Add(15*time.Minute)); err != nil {
		t.Fatalf("successful login after lock expiry: %v", err)
	}
}

func TestLoginSuccessResetsFailures(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	service := NewService(store, credentials)

	// Simulate four transient verification failures followed by a success on
	// the same (client IP, PIN lookup) key.
	failuresLeft := 4
	credentials.verify = func(string, string) (bool, error) {
		if failuresLeft > 0 {
			failuresLeft--
			return false, nil
		}
		return true, nil
	}
	for range 4 {
		if _, _, err := service.Login(context.Background(), "12345678", "10.0.0.1", testNow); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure error = %v", err)
		}
	}
	if _, _, err := service.Login(context.Background(), "12345678", "10.0.0.1", testNow); err != nil {
		t.Fatalf("successful login: %v", err)
	}

	// The success cleared the key. Four more failures must not lock it.
	failuresLeft = 4
	for range 4 {
		if _, _, err := service.Login(context.Background(), "12345678", "10.0.0.1", testNow); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("post-reset failure error = %v", err)
		}
	}
	if _, _, err := service.Login(context.Background(), "12345678", "10.0.0.1", testNow); err != nil {
		t.Fatalf("successful login after reset: %v", err)
	}
}

func TestLoginLimiterStaysBounded(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	service := NewService(store, credentials)
	service.limiter.maxEntries = 3

	for i := range 12 {
		pin := fmt.Sprintf("000000%02d", i)
		if _, _, err := service.Login(context.Background(), pin, "10.0.0.1", testNow); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d error = %v", i, err)
		}
		if size := len(service.limiter.entries); size > service.limiter.maxEntries {
			t.Fatalf("limiter holds %d entries, want at most %d", size, service.limiter.maxEntries)
		}
	}
	service.limiter.entries["stale"] = &limiterEntry{failures: 1, firstSeen: testNow, lastSeen: testNow}
	if _, _, err := service.Login(context.Background(), "00000099", "10.0.0.1", testNow.Add(20*time.Minute)); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("attempt error = %v", err)
	}
	if _, ok := service.limiter.entries["stale"]; ok {
		t.Fatal("stale limiter entry was not pruned")
	}
	if size := len(service.limiter.entries); size > service.limiter.maxEntries {
		t.Fatalf("limiter holds %d entries after pruning", size)
	}
}

func TestAuthenticateReturnsSession(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	store.sessions["digest:session-token"] = Session{User: store.users[1], ExpiresAt: testNow.Add(time.Hour)}
	service := NewService(store, credentials)

	session, err := service.Authenticate(context.Background(), "session-token", testNow)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if session.User.ID != 1 {
		t.Fatalf("unexpected user")
	}
}

func TestAuthenticateRejectsMalformedExpiredAndDeletedSessions(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	service := NewService(store, credentials)

	t.Run("malformed", func(t *testing.T) {
		if _, err := service.Authenticate(context.Background(), "malformed", testNow); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("error = %v, want invalid credentials", err)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		if _, err := service.Authenticate(context.Background(), "session-token", testNow); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("error = %v, want invalid credentials", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		store.ignoreExpiry = true
		store.sessions["digest:session-token"] = Session{User: store.users[1], ExpiresAt: testNow}
		if _, err := service.Authenticate(context.Background(), "session-token", testNow); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("error = %v, want invalid credentials", err)
		}
	})
}

func TestLogoutIsIdempotent(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	service := NewService(store, credentials)

	if err := service.Logout(context.Background(), "session-token"); err != nil {
		t.Fatalf("first logout: %v", err)
	}
	if err := service.Logout(context.Background(), "session-token"); err != nil {
		t.Fatalf("second logout: %v", err)
	}
	if store.deleteCalls != 2 {
		t.Fatalf("delete calls = %d, want 2", store.deleteCalls)
	}
	if err := service.Logout(context.Background(), "malformed"); err != nil {
		t.Fatalf("malformed logout: %v", err)
	}
	if store.deleteCalls != 2 {
		t.Fatal("malformed token reached the store")
	}
}

func TestCreateUserRetriesLookupCollisions(t *testing.T) {
	store := newFakeStore()
	store.duplicateCreates = 2
	credentials := newFakeCredentials("12345678", "hash:12345678")
	pins := []string{"11111111", "22222222", "33333333"}
	index := 0
	credentials.generatePIN = func() (string, error) {
		pin := pins[index]
		index++
		return pin, nil
	}
	service := NewService(store, credentials)

	user, pin, err := service.CreateUser(context.Background(), "  Alice  ")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.ID != 1 || user.DisplayName != "Alice" {
		t.Fatalf("unexpected user %+v", user)
	}
	if pin != "33333333" {
		t.Fatalf("returned PIN is not the successful attempt")
	}
	if store.createCalls != 3 {
		t.Fatalf("create calls = %d, want 3", store.createCalls)
	}
	if got := store.hashes[user.ID]; got != "hash:33333333" {
		t.Fatalf("stored hash = %q", got)
	}
	if _, ok := store.lookups[string(credentials.PINLookup(pin))]; !ok {
		t.Fatal("lookup was not stored")
	}
}

func TestCreateUserGivesUpAfterTenCollisions(t *testing.T) {
	store := newFakeStore()
	store.duplicateCreates = 100
	credentials := newFakeCredentials("12345678", "hash:12345678")
	service := NewService(store, credentials)

	if _, _, err := service.CreateUser(context.Background(), "Alice"); !errors.Is(err, ErrDependency) {
		t.Fatalf("error = %v, want dependency failure", err)
	}
	if store.createCalls != 10 {
		t.Fatalf("create calls = %d, want 10", store.createCalls)
	}
}

func TestCreateUserValidatesDisplayName(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	service := NewService(store, credentials)

	for _, name := range []string{"", "   ", strings.Repeat("x", 101)} {
		if _, _, err := service.CreateUser(context.Background(), name); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("name of %d characters: error = %v, want invalid input", len(name), err)
		}
	}
	if store.createCalls != 0 {
		t.Fatal("invalid name reached the store")
	}
	longUnicode := strings.Repeat("é", 100)
	if _, _, err := service.CreateUser(context.Background(), longUnicode); err != nil {
		t.Fatalf("100-rune unicode name rejected: %v", err)
	}
	if store.createCalls != 1 {
		t.Fatal("valid name was not stored")
	}
}

func TestRotatePINReplacesCredentialAndRevokesSessions(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	generated := []string{"44444444", "55555555"}
	index := 0
	credentials.generatePIN = func() (string, error) {
		pin := generated[index]
		index++
		return pin, nil
	}
	store.duplicateRotates = 1
	store.sessions["digest:old"] = Session{User: store.users[1], ExpiresAt: testNow.Add(time.Hour)}
	service := NewService(store, credentials)

	pin, err := service.RotatePIN(context.Background(), 1)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if pin != "55555555" {
		t.Fatalf("returned PIN is not the successful attempt")
	}
	if store.rotateCalls != 2 {
		t.Fatalf("rotate calls = %d, want 2", store.rotateCalls)
	}
	if _, ok := store.lookups[string(credentials.PINLookup("12345678"))]; ok {
		t.Fatal("old lookup survived rotation")
	}
	if _, ok := store.lookups[string(credentials.PINLookup(pin))]; !ok {
		t.Fatal("new lookup missing")
	}
	if len(store.sessions) != 0 {
		t.Fatal("rotation did not revoke every session")
	}
}

func TestRotatePINReportsMissingUser(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	service := NewService(store, credentials)

	if _, err := service.RotatePIN(context.Background(), 99); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("error = %v, want user not found", err)
	}
}

func TestRevokeUserDisablesAndRevokesSessionsIdempotently(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	store.sessions["digest:old"] = Session{User: store.users[1], ExpiresAt: testNow.Add(time.Hour)}
	service := NewService(store, credentials)

	if err := service.RevokeUser(context.Background(), 1); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if store.users[1].DisabledAt == nil {
		t.Fatal("user was not disabled")
	}
	if len(store.sessions) != 0 {
		t.Fatal("revocation did not delete every session")
	}
	if _, ok := store.lookups[string(credentials.PINLookup("12345678"))]; ok {
		t.Fatal("revoked user still resolves by PIN lookup")
	}
	if err := service.RevokeUser(context.Background(), 1); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if err := service.RevokeUser(context.Background(), 99); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("missing user error = %v, want user not found", err)
	}
}

func TestAdministrationValidatesIDs(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	service := NewService(store, credentials)

	if _, err := service.RotatePIN(context.Background(), 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("rotate 0: error = %v", err)
	}
	if err := service.RevokeUser(context.Background(), -1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("revoke -1: error = %v", err)
	}
	if store.rotateCalls != 0 || store.revokeCalls != 0 {
		t.Fatal("invalid ID reached the store")
	}
}

func TestListUsersPassesThrough(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	store.seed(1, "Alice", "12345678", "hash:12345678", credentials)
	store.seed(2, "Bob", "87654321", "hash:87654321", credentials)
	service := NewService(store, credentials)

	users, err := service.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 2 || users[0].DisplayName != "Alice" || users[1].DisplayName != "Bob" {
		t.Fatalf("unexpected users %+v", users)
	}
}

func TestDependencyFailuresAreCategorized(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	service := NewService(store, credentials)
	store.forceLookupErr = errors.New("connection reset")
	_, _, err := service.Login(context.Background(), "12345678", "10.0.0.1", testNow)
	if !errors.Is(err, ErrDependency) {
		t.Fatalf("login error = %v, want dependency failure", err)
	}
	if strings.Contains(err.Error(), "connection reset") {
		t.Fatal("dependency error leaked adapter detail")
	}

	store.forceSessionErr = errors.New("connection reset")
	if _, err := service.Authenticate(context.Background(), "session-token", testNow); !errors.Is(err, ErrDependency) {
		t.Fatalf("authenticate error = %v, want dependency failure", err)
	}

	store.forceCreateErr = errors.New("connection reset")
	if _, _, err := service.CreateUser(context.Background(), "Alice"); !errors.Is(err, ErrDependency) {
		t.Fatalf("create error = %v, want dependency failure", err)
	}
	store.forceRevokeErr = errors.New("connection reset")
	if err := service.RevokeUser(context.Background(), 1); !errors.Is(err, ErrDependency) {
		t.Fatalf("revoke error = %v, want dependency failure", err)
	}
}

func TestNoGeneratedSecretAppearsInErrors(t *testing.T) {
	store := newFakeStore()
	credentials := newFakeCredentials("12345678", "hash:12345678")
	service := NewService(store, credentials)
	store.forceCreateErr = errors.New("connection reset")
	_, pin, err := service.CreateUser(context.Background(), "Alice")
	if err == nil {
		t.Fatal("expected failure")
	}
	if pin != "" {
		t.Fatalf("a failed create returned a PIN")
	}
	if bytes.Contains([]byte(err.Error()), []byte("12345678")) {
		t.Fatal("error text contains a generated secret")
	}
}
