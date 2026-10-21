package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"nodule-reader/backend/internal/application/auth"
	"nodule-reader/backend/internal/infrastructure/authcrypto"
)

var testCreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func testPepper() string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
}

func newTestService(t *testing.T) (*auth.Service, *memoryStore) {
	t.Helper()
	credentials, err := authcrypto.NewCredentials(testPepper())
	if err != nil {
		t.Fatalf("NewCredentials() error = %v", err)
	}
	store := newMemoryStore()
	return auth.NewService(store, credentials), store
}

func fixedService(service adminService) connectFunc {
	return func(context.Context) (adminService, func(), error) {
		return service, func() {}, nil
	}
}

func runCLI(t *testing.T, connect connectFunc, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, connect)
	return code, stdout.String(), stderr.String()
}

// parseBlocks splits machine-stable output into one map per record.
func parseBlocks(t *testing.T, output string) []map[string]string {
	t.Helper()
	trimmed := strings.TrimSuffix(output, "\n")
	if trimmed == "" {
		return nil
	}
	blocks := strings.Split(trimmed, "\n\n")
	records := make([]map[string]string, 0, len(blocks))
	for _, block := range blocks {
		record := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(line, ": ")
			if !ok {
				t.Fatalf("output line %q is not key: value", line)
			}
			if _, exists := record[key]; exists {
				t.Fatalf("duplicate key %q in block %q", key, block)
			}
			record[key] = value
		}
		records = append(records, record)
	}
	return records
}

func TestUsageErrorsDoNotConnect(t *testing.T) {
	cases := [][]string{
		nil,
		{"bogus"},
		{"create"},
		{"create", "--name", ""},
		{"create", "--name", "Alice", "extra"},
		{"create", "--unknown", "value"},
		{"list", "extra"},
		{"rotate"},
		{"rotate", "--id", "0"},
		{"rotate", "--id", "-4"},
		{"rotate", "--id", "not-a-number"},
		{"rotate", "--id", "1", "extra"},
		{"revoke"},
		{"revoke", "--id", "0"},
		{"revoke", "--id", "-1"},
		{"revoke", "--id", "1", "extra"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			connected := false
			connect := func(context.Context) (adminService, func(), error) {
				connected = true
				return nil, nil, errors.New("must not connect")
			}
			code, stdout, stderr := runCLI(t, connect, args...)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
			}
			if connected {
				t.Fatal("invalid invocation connected to the service")
			}
			if stdout != "" {
				t.Fatalf("usage error wrote to stdout: %q", stdout)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Fatal("usage error produced no stderr message")
			}
		})
	}
}

func TestCreatePrintsOneTimePINAndListRedactsIt(t *testing.T) {
	service, _ := newTestService(t)
	connect := fixedService(service)

	code, stdout, stderr := runCLI(t, connect, "create", "--name", "  Alice  ")
	if code != 0 {
		t.Fatalf("create exit code = %d, stderr = %q", code, stderr)
	}
	records := parseBlocks(t, stdout)
	if len(records) != 1 {
		t.Fatalf("create output blocks = %d, want 1: %q", len(records), stdout)
	}
	record := records[0]
	if record["user_id"] != "1" {
		t.Fatalf("user_id = %q", record["user_id"])
	}
	if record["display_name"] != "Alice" {
		t.Fatalf("display_name = %q, want trimmed Alice", record["display_name"])
	}
	pin := record["pin"]
	if len(pin) != 8 || strings.Trim(pin, "0123456789") != "" {
		t.Fatalf("pin = %q, want eight digits", pin)
	}

	code, stdout, stderr = runCLI(t, connect, "list")
	if code != 0 {
		t.Fatalf("list exit code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, pin) {
		t.Fatalf("list leaked the PIN: %q", stdout)
	}
	for _, forbidden := range []string{"pin:", "pin_lookup", "pin_hash", "session"} {
		if strings.Contains(stdout, forbidden) {
			t.Fatalf("list output contains %q: %q", forbidden, stdout)
		}
	}
	listed := parseBlocks(t, stdout)
	if len(listed) != 1 {
		t.Fatalf("list blocks = %d, want 1: %q", len(listed), stdout)
	}
	user := listed[0]
	want := map[string]string{
		"user_id":      "1",
		"display_name": "Alice",
		"status":       "active",
		"created_at":   "2026-01-02T03:04:05Z",
		"updated_at":   "2026-01-02T03:04:05Z",
	}
	for key, value := range want {
		if user[key] != value {
			t.Fatalf("list %s = %q, want %q (output %q)", key, user[key], value, stdout)
		}
	}
	if len(user) != len(want) {
		t.Fatalf("list record has unexpected fields: %v", user)
	}
}

func TestDuplicateDisplayNamesAreAllowed(t *testing.T) {
	service, _ := newTestService(t)
	connect := fixedService(service)

	code, first, stderr := runCLI(t, connect, "create", "--name", "Same Name")
	if code != 0 {
		t.Fatalf("first create exit code = %d, stderr = %q", code, stderr)
	}
	code, second, stderr := runCLI(t, connect, "create", "--name", "Same Name")
	if code != 0 {
		t.Fatalf("second create exit code = %d, stderr = %q", code, stderr)
	}
	firstRecord := parseBlocks(t, first)[0]
	secondRecord := parseBlocks(t, second)[0]
	if firstRecord["user_id"] == secondRecord["user_id"] {
		t.Fatalf("duplicate names reused user_id %q", firstRecord["user_id"])
	}
	if firstRecord["pin"] == secondRecord["pin"] {
		t.Fatal("duplicate names received the same PIN")
	}
}

func TestDisplayNameBoundsAndUnicode(t *testing.T) {
	service, _ := newTestService(t)
	connect := fixedService(service)

	unicodeName := strings.Repeat("é", 100)
	code, stdout, stderr := runCLI(t, connect, "create", "--name", unicodeName)
	if code != 0 {
		t.Fatalf("100-rune Unicode name exit code = %d, stderr = %q", code, stderr)
	}
	if got := parseBlocks(t, stdout)[0]["display_name"]; got != unicodeName {
		t.Fatalf("display_name = %q, want the Unicode name", got)
	}

	for name, value := range map[string]string{
		"101 runes": strings.Repeat("é", 101),
		"blank":     "   ",
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, connect, "create", "--name", value)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stdout=%q stderr=%q)", code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("invalid name wrote a PIN to stdout: %q", stdout)
			}
			if strings.Contains(stderr, value) && value != "" {
				t.Fatalf("error echoed the display name: %q", stderr)
			}
		})
	}
}

func TestRotateReplacesPINAndRevokeDisablesUser(t *testing.T) {
	service, _ := newTestService(t)
	connect := fixedService(service)

	code, stdout, stderr := runCLI(t, connect, "create", "--name", "Bob")
	if code != 0 {
		t.Fatalf("create exit code = %d, stderr = %q", code, stderr)
	}
	created := parseBlocks(t, stdout)[0]
	userID := created["user_id"]

	code, stdout, stderr = runCLI(t, connect, "rotate", "--id", userID)
	if code != 0 {
		t.Fatalf("rotate exit code = %d, stderr = %q", code, stderr)
	}
	rotated := parseBlocks(t, stdout)[0]
	if rotated["user_id"] != userID {
		t.Fatalf("rotate user_id = %q, want %q", rotated["user_id"], userID)
	}
	if rotated["pin"] == created["pin"] {
		t.Fatal("rotate returned the original PIN")
	}
	if len(rotated) != 2 {
		t.Fatalf("rotate output has unexpected fields: %v", rotated)
	}

	code, stdout, stderr = runCLI(t, connect, "list")
	if code != 0 {
		t.Fatalf("list exit code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, created["pin"]) || strings.Contains(stdout, rotated["pin"]) {
		t.Fatalf("list leaked a PIN: %q", stdout)
	}

	code, stdout, stderr = runCLI(t, connect, "revoke", "--id", userID)
	if code != 0 {
		t.Fatalf("revoke exit code = %d, stderr = %q", code, stderr)
	}
	revoked := parseBlocks(t, stdout)[0]
	if revoked["user_id"] != userID || revoked["status"] != "revoked" {
		t.Fatalf("revoke output = %v", revoked)
	}

	code, stdout, stderr = runCLI(t, connect, "list")
	if code != 0 {
		t.Fatalf("list exit code = %d, stderr = %q", code, stderr)
	}
	if got := parseBlocks(t, stdout)[0]["status"]; got != "revoked" {
		t.Fatalf("status after revoke = %q", got)
	}

	// Revocation is idempotent.
	code, _, stderr = runCLI(t, connect, "revoke", "--id", userID)
	if code != 0 {
		t.Fatalf("second revoke exit code = %d, stderr = %q", code, stderr)
	}
}

func TestAdministrationOperationsReportNotFound(t *testing.T) {
	service, _ := newTestService(t)
	connect := fixedService(service)

	for _, args := range [][]string{
		{"rotate", "--id", "999"},
		{"revoke", "--id", "999"},
	} {
		t.Run(args[0], func(t *testing.T) {
			code, stdout, stderr := runCLI(t, connect, args...)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1 (stdout=%q stderr=%q)", code, stdout, stderr)
			}
			if stdout != "" {
				t.Fatalf("not-found wrote to stdout: %q", stdout)
			}
			if !strings.Contains(strings.ToLower(stderr), "not found") {
				t.Fatalf("stderr = %q, want a not-found message", stderr)
			}
		})
	}
}

func TestMissingOrInvalidPepperFailsWithoutLeakingIt(t *testing.T) {
	setPostgresEnv(t)

	t.Run("missing", func(t *testing.T) {
		t.Setenv("AUTH_PIN_PEPPER", "")
		code, stdout, stderr := runCLI(t, connect, "create", "--name", "Alice")
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stderr=%q)", code, stderr)
		}
		if stdout != "" {
			t.Fatalf("configuration failure wrote to stdout: %q", stdout)
		}
		if !strings.Contains(stderr, "AUTH_PIN_PEPPER") {
			t.Fatalf("stderr = %q, want AUTH_PIN_PEPPER", stderr)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		const invalid = "this-is-not-a-valid-pepper"
		t.Setenv("AUTH_PIN_PEPPER", invalid)
		code, _, stderr := runCLI(t, connect, "create", "--name", "Alice")
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stderr=%q)", code, stderr)
		}
		if strings.Contains(stderr, invalid) {
			t.Fatalf("error leaked the pepper value: %q", stderr)
		}
		if !strings.Contains(stderr, "AUTH_PIN_PEPPER") {
			t.Fatalf("stderr = %q, want AUTH_PIN_PEPPER", stderr)
		}
	})
}

func TestDependencyFailureExitsOne(t *testing.T) {
	connect := func(context.Context) (adminService, func(), error) {
		return nil, nil, errors.New("database unreachable")
	}
	code, stdout, stderr := runCLI(t, connect, "list")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("dependency failure wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "database unreachable") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func setPostgresEnv(t *testing.T) {
	t.Helper()
	t.Setenv("POSTGRES_HOST", "127.0.0.1")
	t.Setenv("POSTGRES_PORT", "5432")
	t.Setenv("POSTGRES_DB", "userctl-test")
	t.Setenv("POSTGRES_USER", "nodule")
	t.Setenv("POSTGRES_PASSWORD", "test-password")
}

// memoryStore is an in-memory auth.Store. Only the administration operations
// used by userctl carry real behaviour; session operations fail closed.
type memoryStore struct {
	nextID  int64
	users   []auth.User
	lookups map[int64][]byte
	hashes  map[int64]string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{lookups: map[int64][]byte{}, hashes: map[int64]string{}}
}

func (s *memoryStore) CreateUser(_ context.Context, displayName string, pinLookup []byte, pinHash string) (auth.User, error) {
	for _, lookup := range s.lookups {
		if bytes.Equal(lookup, pinLookup) {
			return auth.User{}, auth.ErrDuplicatePINLookup
		}
	}
	s.nextID++
	user := auth.User{
		ID:          s.nextID,
		DisplayName: displayName,
		CreatedAt:   testCreatedAt,
		UpdatedAt:   testCreatedAt,
	}
	s.users = append(s.users, user)
	s.lookups[user.ID] = append([]byte(nil), pinLookup...)
	s.hashes[user.ID] = pinHash
	return user, nil
}

func (s *memoryStore) ListUsers(context.Context) ([]auth.User, error) {
	return append([]auth.User(nil), s.users...), nil
}

func (s *memoryStore) FindActiveUserByPINLookup(_ context.Context, pinLookup []byte) (auth.User, string, error) {
	for _, user := range s.users {
		if user.DisabledAt == nil && bytes.Equal(s.lookups[user.ID], pinLookup) {
			return user, s.hashes[user.ID], nil
		}
	}
	return auth.User{}, "", auth.ErrUserNotFound
}

func (s *memoryStore) RotatePIN(_ context.Context, userID int64, pinLookup []byte, pinHash string) error {
	for i, user := range s.users {
		if user.ID != userID {
			continue
		}
		s.lookups[userID] = append([]byte(nil), pinLookup...)
		s.hashes[userID] = pinHash
		s.users[i].UpdatedAt = testCreatedAt.Add(time.Hour)
		return nil
	}
	return auth.ErrUserNotFound
}

func (s *memoryStore) RevokeUser(_ context.Context, userID int64) error {
	for i, user := range s.users {
		if user.ID != userID {
			continue
		}
		if user.DisabledAt == nil {
			disabled := testCreatedAt.Add(2 * time.Hour)
			s.users[i].DisabledAt = &disabled
			s.users[i].UpdatedAt = disabled
		}
		return nil
	}
	return auth.ErrUserNotFound
}

func (s *memoryStore) CreateSession(context.Context, []byte, int64, time.Time) error {
	return errors.New("not implemented")
}

func (s *memoryStore) FindSession(context.Context, []byte, time.Time) (auth.Session, error) {
	return auth.Session{}, auth.ErrSessionNotFound
}

func (s *memoryStore) DeleteSession(context.Context, []byte) error {
	return errors.New("not implemented")
}

func (s *memoryStore) DeleteExpiredSessions(context.Context, time.Time) error {
	return errors.New("not implemented")
}

var _ adminService = (*auth.Service)(nil)
var _ auth.Store = (*memoryStore)(nil)
