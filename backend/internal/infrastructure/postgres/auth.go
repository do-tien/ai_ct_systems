package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"nodule-reader/backend/internal/application/auth"
	"nodule-reader/backend/internal/infrastructure/postgres/lc1db"
)

// AuthStore is the PostgreSQL authentication adapter. It stores PIN lookups,
// Argon2id verifiers and session-token digests only; raw PINs and tokens never
// reach it. Rotation and revocation update the user and delete that user's
// sessions in one transaction.
type AuthStore struct {
	pool *pgxpool.Pool
}

// NewAuthStore builds the authentication persistence boundary on a pool.
func NewAuthStore(pool *pgxpool.Pool) *AuthStore {
	return &AuthStore{pool: pool}
}

// CreateUser inserts a user and reports a duplicate PIN lookup distinctly so
// the service can retry generation.
func (s *AuthStore) CreateUser(ctx context.Context, displayName string, pinLookup []byte, pinHash string) (auth.User, error) {
	row, err := lc1db.New(s.pool).CreateUser(ctx, lc1db.CreateUserParams{
		DisplayName: displayName,
		PinLookup:   pinLookup,
		PinHash:     pinHash,
	})
	if isPINLookupConflict(err) {
		return auth.User{}, auth.ErrDuplicatePINLookup
	}
	if err != nil {
		return auth.User{}, auth.ErrDependency
	}
	return auth.User{
		ID:          row.ID,
		DisplayName: row.DisplayName,
		DisabledAt:  optionalTime(row.DisabledAt),
		CreatedAt:   row.CreatedAt.Time,
		UpdatedAt:   row.UpdatedAt.Time,
	}, nil
}

// ListUsers returns every user, active or disabled, without credential data.
func (s *AuthStore) ListUsers(ctx context.Context) ([]auth.User, error) {
	rows, err := lc1db.New(s.pool).ListUsers(ctx)
	if err != nil {
		return nil, auth.ErrDependency
	}
	users := make([]auth.User, 0, len(rows))
	for _, row := range rows {
		users = append(users, auth.User{
			ID:          row.ID,
			DisplayName: row.DisplayName,
			DisabledAt:  optionalTime(row.DisabledAt),
			CreatedAt:   row.CreatedAt.Time,
			UpdatedAt:   row.UpdatedAt.Time,
		})
	}
	return users, nil
}

// FindActiveUserByPINLookup resolves an active user and its encoded verifier.
// A disabled user is indistinguishable from a missing one.
func (s *AuthStore) FindActiveUserByPINLookup(ctx context.Context, pinLookup []byte) (auth.User, string, error) {
	row, err := lc1db.New(s.pool).FindActiveUserByPINLookup(ctx, pinLookup)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.User{}, "", auth.ErrUserNotFound
	}
	if err != nil {
		return auth.User{}, "", auth.ErrDependency
	}
	return auth.User{ID: row.ID, DisplayName: row.DisplayName}, row.PinHash, nil
}

// RotatePIN replaces the credential and deletes every session of the user in
// one transaction.
func (s *AuthStore) RotatePIN(ctx context.Context, userID int64, pinLookup []byte, pinHash string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return auth.ErrDependency
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := lc1db.New(s.pool).WithTx(tx)
	_, err = queries.RotateUserPIN(ctx, lc1db.RotateUserPINParams{
		ID:        userID,
		PinLookup: pinLookup,
		PinHash:   pinHash,
	})
	switch {
	case isPINLookupConflict(err):
		return auth.ErrDuplicatePINLookup
	case errors.Is(err, pgx.ErrNoRows):
		return auth.ErrUserNotFound
	case err != nil:
		return auth.ErrDependency
	}
	if err := queries.DeleteSessionsForUser(ctx, userID); err != nil {
		return auth.ErrDependency
	}
	if err := tx.Commit(ctx); err != nil {
		return auth.ErrDependency
	}
	return nil
}

// RevokeUser disables the user and deletes every session in one transaction.
// Repeating the operation for an already disabled user succeeds.
func (s *AuthStore) RevokeUser(ctx context.Context, userID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return auth.ErrDependency
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := lc1db.New(s.pool).WithTx(tx)
	if _, err := queries.DisableUser(ctx, userID); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return auth.ErrDependency
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
			return auth.ErrDependency
		}
		if !exists {
			return auth.ErrUserNotFound
		}
	}
	if err := queries.DeleteSessionsForUser(ctx, userID); err != nil {
		return auth.ErrDependency
	}
	if err := tx.Commit(ctx); err != nil {
		return auth.ErrDependency
	}
	return nil
}

// CreateSession stores a token digest with its absolute expiry.
func (s *AuthStore) CreateSession(ctx context.Context, tokenDigest []byte, userID int64, expiresAt time.Time) error {
	err := lc1db.New(s.pool).CreateSession(ctx, lc1db.CreateSessionParams{
		TokenDigest: tokenDigest,
		UserID:      userID,
		ExpiresAt:   pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return auth.ErrDependency
	}
	return nil
}

// FindSession resolves an unexpired session of an active user. Missing,
// expired and disabled-user sessions all report ErrSessionNotFound.
func (s *AuthStore) FindSession(ctx context.Context, tokenDigest []byte, now time.Time) (auth.Session, error) {
	row, err := lc1db.New(s.pool).FindSession(ctx, lc1db.FindSessionParams{
		TokenDigest: tokenDigest,
		ExpiresAt:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Session{}, auth.ErrSessionNotFound
	}
	if err != nil {
		return auth.Session{}, auth.ErrDependency
	}
	return auth.Session{
		User:      auth.User{ID: row.UserID, DisplayName: row.DisplayName},
		ExpiresAt: row.ExpiresAt.Time,
	}, nil
}

// DeleteSession removes one session. A missing session is not an error.
func (s *AuthStore) DeleteSession(ctx context.Context, tokenDigest []byte) error {
	if err := lc1db.New(s.pool).DeleteSession(ctx, tokenDigest); err != nil {
		return auth.ErrDependency
	}
	return nil
}

// DeleteExpiredSessions removes every session at or before now.
func (s *AuthStore) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	if err := lc1db.New(s.pool).DeleteExpiredSessions(ctx, pgtype.Timestamptz{Time: now, Valid: true}); err != nil {
		return auth.ErrDependency
	}
	return nil
}

func optionalTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	timestamp := value.Time
	return &timestamp
}

// isPINLookupConflict reports only the unique violation of users.pin_lookup.
func isPINLookupConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "users_pin_lookup_key"
}
