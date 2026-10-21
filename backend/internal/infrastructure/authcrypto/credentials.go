// Package authcrypto implements the PIN lookup, Argon2id verifier and opaque
// session-token primitives of the authentication boundary. It never persists
// or logs secret material.
package authcrypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	// PINLength is the number of decimal digits in a generated PIN.
	PINLength = 8
	// lookupSize is the HMAC-SHA-256 output length in bytes.
	lookupSize = 32
	// tokenSize is the raw session-token length in bytes.
	tokenSize = 32

	argonVersion     = 19
	argonMemory      = 64 * 1024 // kibibytes
	argonIterations  = 3
	argonParallelism = 1
	argonSaltSize    = 16
	argonKeySize     = 32
)

// Credentials performs every cryptographic operation over PINs and session
// tokens. The pepper is decoded and validated once at construction.
type Credentials struct {
	pepper []byte
	random io.Reader
}

// NewCredentials validates a base64url-encoded 32-byte pepper and uses
// crypto/rand for all generated values.
func NewCredentials(pepperBase64 string) (*Credentials, error) {
	return NewCredentialsWithRandom(pepperBase64, rand.Reader)
}

// NewCredentialsWithRandom is NewCredentials with an injectable random source.
// Tests use it to make generation deterministic.
func NewCredentialsWithRandom(pepperBase64 string, random io.Reader) (*Credentials, error) {
	if random == nil {
		return nil, errors.New("authcrypto: random source is required")
	}
	pepper, err := base64.RawURLEncoding.DecodeString(pepperBase64)
	if err != nil {
		return nil, errors.New("authcrypto: AUTH_PIN_PEPPER is not valid unpadded base64url")
	}
	if len(pepper) != 32 {
		return nil, errors.New("authcrypto: AUTH_PIN_PEPPER must decode to exactly 32 bytes")
	}
	return &Credentials{pepper: append([]byte(nil), pepper...), random: random}, nil
}

// GeneratePIN returns an eight-digit PIN, including leading zeroes, with every
// value in the 100,000,000-value domain equally likely.
func (c *Credentials) GeneratePIN() (string, error) {
	pin := make([]byte, PINLength)
	filled := 0
	buffer := make([]byte, PINLength*2)
	for filled < PINLength {
		if _, err := io.ReadFull(c.random, buffer); err != nil {
			return "", fmt.Errorf("authcrypto: generate PIN: %w", err)
		}
		for _, value := range buffer {
			// 250 is the largest multiple of 10 in a byte; values above it are
			// rejected so modulo reduction stays unbiased.
			if value >= 250 {
				continue
			}
			pin[filled] = '0' + value%10
			filled++
			if filled == PINLength {
				break
			}
		}
	}
	return string(pin), nil
}

// PINLookup returns the deterministic HMAC-SHA-256 lookup of a PIN under the
// configured pepper.
func (c *Credentials) PINLookup(pin string) []byte {
	mac := hmac.New(sha256.New, c.pepper)
	mac.Write([]byte(pin))
	return mac.Sum(nil)
}

// HashPIN encodes an Argon2id verifier over the peppered PIN lookup.
func (c *Credentials) HashPIN(pin string) (string, error) {
	salt := make([]byte, argonSaltSize)
	if _, err := io.ReadFull(c.random, salt); err != nil {
		return "", fmt.Errorf("authcrypto: generate salt: %w", err)
	}
	key := argon2.IDKey(c.PINLookup(pin), salt, argonIterations, argonMemory, argonParallelism, argonKeySize)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argonVersion,
		argonMemory,
		argonIterations,
		argonParallelism,
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(key),
	), nil
}

// VerifyPIN checks a PIN against an encoded Argon2id verifier. Unsupported or
// malformed encodings are rejected instead of being weakened.
func (c *Credentials) VerifyPIN(pin, encoded string) (bool, error) {
	salt, expected, err := decodeVerifier(encoded)
	if err != nil {
		return false, err
	}
	actual := argon2.IDKey(c.PINLookup(pin), salt, argonIterations, argonMemory, argonParallelism, argonKeySize)
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

// GenerateSessionToken returns 32 random bytes as an unpadded base64url token
// together with the SHA-256 digest that may be stored.
func (c *Credentials) GenerateSessionToken() (string, []byte, error) {
	raw := make([]byte, tokenSize)
	if _, err := io.ReadFull(c.random, raw); err != nil {
		return "", nil, fmt.Errorf("authcrypto: generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, tokenDigest(token), nil
}

// SessionDigest validates an unpadded base64url token carrying 32 bytes and
// returns its SHA-256 digest. Malformed tokens are rejected.
func (c *Credentials) SessionDigest(raw string) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("authcrypto: empty session token")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("authcrypto: malformed session token")
	}
	if len(decoded) != tokenSize {
		return nil, errors.New("authcrypto: malformed session token")
	}
	return tokenDigest(raw), nil
}

func tokenDigest(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// decodeVerifier parses the fixed PHC shape and rejects any parameter that
// differs from the supported Argon2id profile.
func decodeVerifier(encoded string) ([]byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return nil, nil, errors.New("authcrypto: unsupported verifier encoding")
	}
	if parts[1] != "argon2id" {
		return nil, nil, errors.New("authcrypto: unsupported verifier algorithm")
	}
	if parts[2] != "v="+strconv.Itoa(argonVersion) {
		return nil, nil, errors.New("authcrypto: unsupported verifier version")
	}
	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 {
		return nil, nil, errors.New("authcrypto: unsupported verifier parameters")
	}
	want := []string{
		"m=" + strconv.Itoa(argonMemory),
		"t=" + strconv.Itoa(argonIterations),
		"p=" + strconv.Itoa(argonParallelism),
	}
	for i, parameter := range parameters {
		if parameter != want[i] {
			return nil, nil, errors.New("authcrypto: unsupported verifier parameters")
		}
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != argonSaltSize {
		return nil, nil, errors.New("authcrypto: malformed verifier salt")
	}
	hash, err := base64.RawURLEncoding.DecodeString(parts[5])
	if err != nil || len(hash) != argonKeySize {
		return nil, nil, errors.New("authcrypto: malformed verifier hash")
	}
	return salt, hash, nil
}
