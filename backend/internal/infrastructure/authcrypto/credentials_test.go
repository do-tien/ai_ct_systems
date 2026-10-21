package authcrypto_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"nodule-reader/backend/internal/infrastructure/authcrypto"
)

func testPepper(t *testing.T) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))
}

func newCredentials(t *testing.T) *authcrypto.Credentials {
	t.Helper()
	credentials, err := authcrypto.NewCredentials(testPepper(t))
	if err != nil {
		t.Fatalf("build credentials: %v", err)
	}
	return credentials
}

func TestNewCredentialsRejectsMalformedPepper(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"not base64":   "not base64!",
		"short":        base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31)),
		"long":         base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 33)),
		"empty decode": "====",
	}
	for name, pepper := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := authcrypto.NewCredentials(pepper); err == nil {
				t.Fatalf("malformed pepper accepted")
			}
		})
	}
}

func TestNewCredentialsAcceptsBase64URLPepper(t *testing.T) {
	credentials := newCredentials(t)
	if credentials == nil {
		t.Fatal("nil credentials")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// rejectThenSequence emits rejected bytes before the successful values, so the
// generator must use rejection sampling rather than consuming them directly.
type rejectThenSequence struct {
	prefix []byte
	data   []byte
}

func (r *rejectThenSequence) Read(p []byte) (int, error) {
	for i := range p {
		switch {
		case len(r.prefix) > 0:
			p[i] = r.prefix[0]
			r.prefix = r.prefix[1:]
		case len(r.data) > 0:
			p[i] = r.data[0]
			r.data = r.data[1:]
		default:
			p[i] = 0
		}
	}
	return len(p), nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("random source failed") }

func TestGeneratePINHasEightDigitsAndPreservesLeadingZeroes(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9]{8}$`)
	credentials := newCredentials(t)
	seenLeadingZero := false
	for range 64 {
		pin, err := credentials.GeneratePIN()
		if err != nil {
			t.Fatalf("generate PIN: %v", err)
		}
		if !pattern.MatchString(pin) {
			t.Fatalf("generated PIN does not have eight digits")
		}
		if strings.HasPrefix(pin, "0") {
			seenLeadingZero = true
		}
	}
	if !seenLeadingZero {
		t.Fatalf("64 generated PINs contained no leading zero")
	}
}

func TestGeneratePINUsesRejectionSampling(t *testing.T) {
	credentials, err := authcrypto.NewCredentialsWithRandom(testPepper(t), &rejectThenSequence{
		prefix: []byte{250, 251, 255, 250},
		data:   []byte{0, 1, 2, 3, 4, 5, 6, 7},
	})
	if err != nil {
		t.Fatalf("build credentials: %v", err)
	}
	pin, err := credentials.GeneratePIN()
	if err != nil {
		t.Fatalf("generate PIN: %v", err)
	}
	if pin != "01234567" {
		t.Fatalf("PIN = %q, want the first eight accepted digits", pin)
	}

	zeroes, err := authcrypto.NewCredentialsWithRandom(testPepper(t), zeroReader{})
	if err != nil {
		t.Fatalf("build credentials: %v", err)
	}
	pin, err = zeroes.GeneratePIN()
	if err != nil {
		t.Fatalf("generate PIN: %v", err)
	}
	if pin != "00000000" {
		t.Fatalf("PIN = %q, want leading zeroes preserved", pin)
	}
}

func TestGeneratePINPropagatesRandomFailure(t *testing.T) {
	credentials, err := authcrypto.NewCredentialsWithRandom(testPepper(t), failingReader{})
	if err != nil {
		t.Fatalf("build credentials: %v", err)
	}
	if _, err := credentials.GeneratePIN(); err == nil {
		t.Fatal("random failure was swallowed")
	}
}

func TestPINLookupIsDeterministicAndPepperBound(t *testing.T) {
	credentials := newCredentials(t)
	other, err := authcrypto.NewCredentials(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa5}, 32)))
	if err != nil {
		t.Fatalf("build credentials: %v", err)
	}
	first := credentials.PINLookup("01234567")
	second := credentials.PINLookup("01234567")
	if len(first) != 32 {
		t.Fatalf("lookup length = %d, want 32", len(first))
	}
	if !bytes.Equal(first, second) {
		t.Fatal("lookup is not deterministic")
	}
	if bytes.Equal(credentials.PINLookup("01234567"), credentials.PINLookup("01234568")) {
		t.Fatal("different PINs share a lookup")
	}
	if bytes.Equal(first, other.PINLookup("01234567")) {
		t.Fatal("lookup is independent of the pepper")
	}
}

func TestHashPINProducesArgon2idVerifierWithFreshSalt(t *testing.T) {
	credentials := newCredentials(t)
	pin := "01234567"
	first, err := credentials.HashPIN(pin)
	if err != nil {
		t.Fatalf("hash PIN: %v", err)
	}
	second, err := credentials.HashPIN(pin)
	if err != nil {
		t.Fatalf("hash PIN: %v", err)
	}
	if first == second {
		t.Fatal("equal PINs share an encoded verifier; salt is not fresh")
	}
	pattern := regexp.MustCompile(`^\$argon2id\$v=19\$m=65536,t=3,p=1\$([A-Za-z0-9_-]{22})\$([A-Za-z0-9_-]{43})$`)
	match := pattern.FindStringSubmatch(first)
	if match == nil {
		t.Fatalf("encoded verifier does not carry the fixed parameters")
	}
	salt, err := base64.RawURLEncoding.DecodeString(match[1])
	if err != nil || len(salt) != 16 {
		t.Fatalf("salt is not 16 bytes")
	}
	hash, err := base64.RawURLEncoding.DecodeString(match[2])
	if err != nil || len(hash) != 32 {
		t.Fatalf("verifier is not 32 bytes")
	}

	for _, encoded := range []string{first, second} {
		ok, err := credentials.VerifyPIN(pin, encoded)
		if err != nil || !ok {
			t.Fatalf("verification of a generated verifier failed")
		}
	}
	ok, err := credentials.VerifyPIN("01234568", first)
	if err != nil {
		t.Fatalf("verify wrong PIN: %v", err)
	}
	if ok {
		t.Fatal("wrong PIN verified")
	}
}

func TestPINBoundariesHashAndVerify(t *testing.T) {
	credentials := newCredentials(t)
	for _, pin := range []string{"00000000", "00000001", "49999999", "99999998", "99999999"} {
		encoded, err := credentials.HashPIN(pin)
		if err != nil {
			t.Fatalf("hash boundary PIN: %v", err)
		}
		ok, err := credentials.VerifyPIN(pin, encoded)
		if err != nil || !ok {
			t.Fatalf("boundary PIN did not verify")
		}
		if ok, err := credentials.VerifyPIN("12345678", encoded); err != nil || ok {
			t.Fatalf("a different PIN verified against a boundary verifier")
		}
	}
}

func TestVerifyPINRejectsUnsupportedOrTamperedEncoding(t *testing.T) {
	credentials := newCredentials(t)
	valid, err := credentials.HashPIN("01234567")
	if err != nil {
		t.Fatalf("hash PIN: %v", err)
	}
	parts := strings.Split(valid, "$")
	if len(parts) != 6 {
		t.Fatalf("unexpected encoded verifier shape")
	}
	with := func(index int, value string) string {
		copied := append([]string(nil), parts...)
		copied[index] = value
		return strings.Join(copied, "$")
	}

	cases := map[string]string{
		"unsupported version": with(2, "v=16"),
		"weakened memory":     with(3, "m=32768,t=3,p=1"),
		"fewer iterations":    with(3, "m=65536,t=2,p=1"),
		"parallelism":         with(3, "m=65536,t=3,p=2"),
		"extra parameter":     with(3, "m=65536,t=3,p=1,x=1"),
		"wrong algorithm":     with(1, "argon2i"),
		"short salt":          with(4, "AAAA"),
		"short hash":          with(5, "AAAA"),
		"invalid salt":        with(4, "!!!!"),
		"invalid hash":        with(5, "!!!!"),
		"missing salt":        with(4, ""),
		"missing hash":        with(5, ""),
		"empty":               "",
		"no prefix":           "argon2id",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := credentials.VerifyPIN("01234567", encoded); err == nil {
				t.Fatalf("unsupported verifier accepted")
			}
		})
	}
}

func TestVerifyPINTamperedDigestReturnsFalse(t *testing.T) {
	credentials := newCredentials(t)
	valid, err := credentials.HashPIN("01234567")
	if err != nil {
		t.Fatalf("hash PIN: %v", err)
	}
	parts := strings.Split(valid, "$")
	digest := []byte(parts[5])
	if digest[0] == 'A' {
		digest[0] = 'B'
	} else {
		digest[0] = 'A'
	}
	tampered := strings.Join([]string{parts[0], parts[1], parts[2], parts[3], parts[4], string(digest)}, "$")
	ok, err := credentials.VerifyPIN("01234567", tampered)
	if err != nil {
		t.Fatalf("tampered verifier returned an error: %v", err)
	}
	if ok {
		t.Fatal("tampered verifier accepted")
	}
}

func TestGenerateSessionTokenAndDigest(t *testing.T) {
	credentials := newCredentials(t)
	raw, digest, err := credentials.GenerateSessionToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("token is not unpadded base64url: %v", err)
	}
	if len(decoded) != 32 {
		t.Fatalf("token carries %d bytes, want 32", len(decoded))
	}
	if len(digest) != 32 {
		t.Fatalf("digest length = %d, want 32", len(digest))
	}
	again, err := credentials.SessionDigest(raw)
	if err != nil {
		t.Fatalf("session digest: %v", err)
	}
	if !bytes.Equal(digest, again) {
		t.Fatal("session digest is not deterministic")
	}
	other, _, err := credentials.GenerateSessionToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if raw == other {
		t.Fatal("two generated tokens are equal")
	}

	for _, malformed := range []string{"", "not a token", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31))} {
		if _, err := credentials.SessionDigest(malformed); err == nil {
			t.Fatalf("malformed token accepted: %q", malformed)
		}
	}
}

func TestGenerateSessionTokenPropagatesRandomFailure(t *testing.T) {
	credentials, err := authcrypto.NewCredentialsWithRandom(testPepper(t), failingReader{})
	if err != nil {
		t.Fatalf("build credentials: %v", err)
	}
	if _, _, err := credentials.GenerateSessionToken(); err == nil {
		t.Fatal("random failure was swallowed")
	}
}

var _ io.Reader = zeroReader{}
