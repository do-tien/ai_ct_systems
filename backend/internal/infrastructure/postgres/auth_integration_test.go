package postgres_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"nodule-reader/backend/internal/application/auth"
	"nodule-reader/backend/internal/infrastructure/postgres"
	"nodule-reader/backend/internal/infrastructure/postgres/lc1db"
	"nodule-reader/backend/migrations"
)

// migrateTo opens the same goose provider the migrate command uses and applies
// migrations up to and including version.
func migrateTo(t *testing.T, h *storageHarness, version int64) *goose.Provider {
	t.Helper()
	db, err := sql.Open("pgx", h.ownerDSN)
	if err != nil {
		t.Fatalf("open owner handle: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.Files)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	t.Cleanup(func() { provider.Close() })
	ctx := context.Background()
	if version < 0 {
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("migrate up: %v", err)
		}
		return provider
	}
	if _, err := provider.UpTo(ctx, version); err != nil {
		t.Fatalf("migrate up to %d: %v", version, err)
	}
	return provider
}

// syntheticLookup builds a distinct 32-byte PIN lookup value without using any
// real credential material.
func syntheticLookup(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, 32)
}

func tableColumns(t *testing.T, conn *pgx.Conn, table string) map[string]struct {
	Nullable  bool
	DataType  string
	Generated bool
} {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT column_name, is_nullable, data_type, is_generated
		   FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = $1`, table)
	if err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	defer rows.Close()
	columns := map[string]struct {
		Nullable  bool
		DataType  string
		Generated bool
	}{}
	for rows.Next() {
		var name, nullable, dataType, generated string
		if err := rows.Scan(&name, &nullable, &dataType, &generated); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		columns[name] = struct {
			Nullable  bool
			DataType  string
			Generated bool
		}{Nullable: nullable == "YES", DataType: dataType, Generated: generated != "NEVER"}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columns of %s: %v", table, err)
	}
	return columns
}

func requireColumn(t *testing.T, columns map[string]struct {
	Nullable  bool
	DataType  string
	Generated bool
}, name string) {
	t.Helper()
	if _, ok := columns[name]; !ok {
		t.Fatalf("missing column %q in %v", name, columns)
	}
}

func TestAuthMigrationSchema(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	owner := h.owner(t)

	t.Run("users", func(t *testing.T) {
		columns := tableColumns(t, owner, "users")
		for _, name := range []string{"id", "display_name", "pin_lookup", "pin_hash", "disabled_at", "created_at", "updated_at"} {
			requireColumn(t, columns, name)
		}
		if columns["pin_lookup"].Nullable {
			t.Fatalf("pin_lookup must be NOT NULL")
		}
		if columns["pin_hash"].Nullable {
			t.Fatalf("pin_hash must be NOT NULL")
		}
		if !columns["disabled_at"].Nullable {
			t.Fatalf("disabled_at must be nullable")
		}
	})

	t.Run("auth_sessions", func(t *testing.T) {
		columns := tableColumns(t, owner, "auth_sessions")
		for _, name := range []string{"token_digest", "user_id", "created_at", "expires_at"} {
			requireColumn(t, columns, name)
		}
		if columns["token_digest"].Nullable {
			t.Fatalf("token_digest must be NOT NULL")
		}
	})

	t.Run("reads_ownership", func(t *testing.T) {
		columns := tableColumns(t, owner, "reads")
		requireColumn(t, columns, "reader_user_id")
		if !columns["reader_user_id"].Nullable {
			t.Fatalf("reads.reader_user_id must stay nullable until the ownership cutover")
		}
		if _, ok := columns["reader"]; !ok {
			t.Fatalf("reads.reader must remain the authoritative identity column")
		}
	})

	t.Run("events_actor", func(t *testing.T) {
		columns := tableColumns(t, owner, "events")
		requireColumn(t, columns, "actor_user_id")
		if !columns["actor_user_id"].Nullable {
			t.Fatalf("events.actor_user_id must be nullable for historical events")
		}
		if _, ok := columns["actor"]; !ok {
			t.Fatalf("events.actor snapshot missing")
		}
	})

	t.Run("foreign_key_delete_rules", func(t *testing.T) {
		ctx := context.Background()
		rules := map[string]string{}
		rows, err := owner.Query(ctx, `
			SELECT source.relname || '.' || a.attname, c.confdeltype::text
			  FROM pg_constraint c
			  JOIN pg_class source ON source.oid = c.conrelid
			  JOIN unnest(c.conkey) AS k(attnum) ON true
			  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
			 WHERE c.contype = 'f' AND source.relname IN ('reads', 'events', 'auth_sessions')`)
		if err != nil {
			t.Fatalf("foreign keys: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var column, rule string
			if err := rows.Scan(&column, &rule); err != nil {
				t.Fatalf("scan fk: %v", err)
			}
			rules[column] = rule
		}
		if got := rules["auth_sessions.user_id"]; got != "c" {
			t.Fatalf("auth_sessions.user_id delete rule = %q, want cascade", got)
		}
		if got := rules["reads.reader_user_id"]; got != "a" {
			t.Fatalf("reads.reader_user_id delete rule = %q, want no action", got)
		}
		if got := rules["events.actor_user_id"]; got != "n" {
			t.Fatalf("events.actor_user_id delete rule = %q, want set null", got)
		}
	})
}

func TestAuthPINLookupUnique(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	owner := h.owner(t)
	ctx := context.Background()

	lookup := syntheticLookup(0x11)
	if _, err := owner.Exec(ctx,
		`INSERT INTO users (display_name, pin_lookup, pin_hash) VALUES ('first', $1, 'hash')`, lookup); err != nil {
		t.Fatalf("insert first user: %v", err)
	}
	_, err := owner.Exec(ctx,
		`INSERT INTO users (display_name, pin_lookup, pin_hash) VALUES ('second', $1, 'hash')`, lookup)
	assertSQLState(t, err, "23505", "duplicate pin_lookup")

	// Display names may duplicate; only the lookup must be unique.
	if _, err := owner.Exec(ctx,
		`INSERT INTO users (display_name, pin_lookup, pin_hash) VALUES ('first', $1, 'hash')`, syntheticLookup(0x12)); err != nil {
		t.Fatalf("duplicate display name rejected: %v", err)
	}
}

func TestAuthUserQueries(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	owner := h.owner(t)
	q := lc1db.New(owner)
	ctx := context.Background()

	first, err := q.CreateUser(ctx, lc1db.CreateUserParams{
		DisplayName: "BS. Nguyễn Văn A",
		PinLookup:   syntheticLookup(0x21),
		PinHash:     "$argon2id$v=19$synthetic",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if first.ID <= 0 || first.DisplayName != "BS. Nguyễn Văn A" || first.DisabledAt.Valid {
		t.Fatalf("created user = %+v", first)
	}

	if _, err := q.CreateUser(ctx, lc1db.CreateUserParams{
		DisplayName: "BS. Nguyễn Văn A",
		PinLookup:   syntheticLookup(0x22),
		PinHash:     "$argon2id$v=19$synthetic-two",
	}); err != nil {
		t.Fatalf("CreateUser duplicate name: %v", err)
	}

	users, err := q.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("ListUsers returned %d users, want 2", len(users))
	}
	if users[0].ID != first.ID {
		t.Fatalf("ListUsers order = %v, want id ascending", users)
	}

	active, err := q.FindActiveUserByPINLookup(ctx, syntheticLookup(0x21))
	if err != nil {
		t.Fatalf("FindActiveUserByPINLookup: %v", err)
	}
	if active.ID != first.ID || active.PinHash != "$argon2id$v=19$synthetic" {
		t.Fatalf("active user = %+v", active)
	}

	rotated, err := q.RotateUserPIN(ctx, lc1db.RotateUserPINParams{
		ID:        first.ID,
		PinLookup: syntheticLookup(0x23),
		PinHash:   "$argon2id$v=19$rotated",
	})
	if err != nil {
		t.Fatalf("RotateUserPIN: %v", err)
	}
	if !rotated.UpdatedAt.Time.After(first.UpdatedAt.Time) && !rotated.UpdatedAt.Time.Equal(first.UpdatedAt.Time) {
		t.Fatalf("rotate did not advance updated_at: %+v", rotated)
	}
	if _, err := q.FindActiveUserByPINLookup(ctx, syntheticLookup(0x21)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old lookup still resolves, error = %v", err)
	}
	if _, err := q.FindActiveUserByPINLookup(ctx, syntheticLookup(0x23)); err != nil {
		t.Fatalf("rotated lookup does not resolve: %v", err)
	}

	disabled, err := q.DisableUser(ctx, first.ID)
	if err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if !disabled.DisabledAt.Valid {
		t.Fatalf("DisableUser did not set disabled_at")
	}
	if _, err := q.FindActiveUserByPINLookup(ctx, syntheticLookup(0x23)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disabled user still resolves, error = %v", err)
	}
	if _, err := q.DisableUser(ctx, first.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second DisableUser error = %v, want ErrNoRows", err)
	}
	if _, err := q.DisableUser(ctx, 999999); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("DisableUser missing error = %v, want ErrNoRows", err)
	}
}

func TestAuthSessionQueries(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	owner := h.owner(t)
	q := lc1db.New(owner)
	ctx := context.Background()

	user, err := q.CreateUser(ctx, lc1db.CreateUserParams{
		DisplayName: "session-user",
		PinLookup:   syntheticLookup(0x31),
		PinHash:     "synthetic",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	digest := bytes.Repeat([]byte{0x41}, 32)
	expires := now.Add(8 * time.Hour)

	if err := q.CreateSession(ctx, lc1db.CreateSessionParams{
		TokenDigest: digest,
		UserID:      user.ID,
		ExpiresAt:   pgtype.Timestamptz{Time: expires, Valid: true},
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	session, err := q.FindSession(ctx, lc1db.FindSessionParams{
		TokenDigest: digest,
		ExpiresAt:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		t.Fatalf("FindSession: %v", err)
	}
	if session.UserID != user.ID || session.DisplayName != "session-user" || !session.ExpiresAt.Time.Equal(expires) {
		t.Fatalf("session = %+v", session)
	}

	// A session is invalid at its absolute expiry and is removed by the expiry
	// sweep, never extended by a lookup.
	if _, err := q.FindSession(ctx, lc1db.FindSessionParams{
		TokenDigest: digest,
		ExpiresAt:   pgtype.Timestamptz{Time: expires, Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired session resolved, error = %v", err)
	}

	expiredDigest := bytes.Repeat([]byte{0x42}, 32)
	if _, err := owner.Exec(ctx,
		`INSERT INTO auth_sessions (token_digest, user_id, created_at, expires_at)
		 VALUES ($1, $2, now() - interval '2 hours', now() - interval '1 hour')`,
		expiredDigest, user.ID); err != nil {
		t.Fatalf("CreateSession expired fixture: %v", err)
	}

	if err := q.DeleteExpiredSessions(ctx, pgtype.Timestamptz{Time: now, Valid: true}); err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if _, err := q.FindSession(ctx, lc1db.FindSessionParams{
		TokenDigest: expiredDigest,
		ExpiresAt:   pgtype.Timestamptz{Time: now.Add(-48 * time.Hour), Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired session survived sweep, error = %v", err)
	}
	// The unexpired session remains.
	if _, err := q.FindSession(ctx, lc1db.FindSessionParams{
		TokenDigest: digest,
		ExpiresAt:   pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		t.Fatalf("unexpired session removed by sweep: %v", err)
	}

	if err := q.DeleteSession(ctx, digest); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := q.DeleteSession(ctx, digest); err != nil {
		t.Fatalf("idempotent DeleteSession: %v", err)
	}
	if _, err := q.FindSession(ctx, lc1db.FindSessionParams{
		TokenDigest: digest,
		ExpiresAt:   pgtype.Timestamptz{Time: now, Valid: true},
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted session resolved, error = %v", err)
	}

	second := bytes.Repeat([]byte{0x43}, 32)
	third := bytes.Repeat([]byte{0x44}, 32)
	for _, digest := range [][]byte{second, third} {
		if err := q.CreateSession(ctx, lc1db.CreateSessionParams{
			TokenDigest: digest,
			UserID:      user.ID,
			ExpiresAt:   pgtype.Timestamptz{Time: expires, Valid: true},
		}); err != nil {
			t.Fatalf("CreateSession fixture: %v", err)
		}
	}
	if err := q.DeleteSessionsForUser(ctx, user.ID); err != nil {
		t.Fatalf("DeleteSessionsForUser: %v", err)
	}
	var remaining int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE user_id = $1`, user.ID).Scan(&remaining); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("DeleteSessionsForUser left %d sessions", remaining)
	}
}

func TestAuthSessionForeignKeys(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	owner := h.owner(t)
	q := lc1db.New(owner)
	ctx := context.Background()

	digest := bytes.Repeat([]byte{0x51}, 32)
	err := q.CreateSession(ctx, lc1db.CreateSessionParams{
		TokenDigest: digest,
		UserID:      999999,
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	assertSQLState(t, err, "23503", "session without user")

	user, err := q.CreateUser(ctx, lc1db.CreateUserParams{
		DisplayName: "fk-user",
		PinLookup:   syntheticLookup(0x52),
		PinHash:     "synthetic",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := q.CreateSession(ctx, lc1db.CreateSessionParams{
		TokenDigest: digest,
		UserID:      user.ID,
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := owner.Exec(ctx, `DELETE FROM users WHERE id = $1`, user.ID); err != nil {
		t.Fatalf("delete user with sessions: %v", err)
	}
	var sessions int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM auth_sessions WHERE user_id = $1`, user.ID).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Fatalf("user delete left %d sessions, want cascade", sessions)
	}

	// A read owner cannot be deleted while clinical rows reference it.
	readUser, err := q.CreateUser(ctx, lc1db.CreateUserParams{
		DisplayName: "read-owner",
		PinLookup:   syntheticLookup(0x53),
		PinHash:     "synthetic",
	})
	if err != nil {
		t.Fatalf("CreateUser read owner: %v", err)
	}
	caseRow := createCase(t, q, "1.2.3.4.90")
	read, err := q.CreateRead(ctx, lc1db.CreateReadParams{
		CaseID: caseRow.ID,
		Kind:   "initial",
		Reader: readUser.DisplayName,
	})
	if err != nil {
		t.Fatalf("CreateRead: %v", err)
	}
	if read.ReaderUserID.Valid {
		t.Fatalf("new read owner = %+v, want null", read.ReaderUserID)
	}
	// A backfilled row carries an explicit owner and restricts user deletion.
	if _, err := owner.Exec(ctx,
		`INSERT INTO reads (case_id, kind, reader, reader_user_id) VALUES ($1, 'final', 'read-owner', $2)`,
		caseRow.ID, readUser.ID); err != nil {
		t.Fatalf("seed owned read: %v", err)
	}
	_, err = owner.Exec(ctx, `DELETE FROM users WHERE id = $1`, readUser.ID)
	assertSQLState(t, err, "23503", "delete read owner")

	// Historical event actors are preserved with a null user id.
	eventUser, err := q.CreateUser(ctx, lc1db.CreateUserParams{
		DisplayName: "event-actor",
		PinLookup:   syntheticLookup(0x54),
		PinHash:     "synthetic",
	})
	if err != nil {
		t.Fatalf("CreateUser event actor: %v", err)
	}
	event, err := q.AppendEvent(ctx, lc1db.AppendEventParams{
		CaseID:      pgtype.Int8{Int64: caseRow.ID, Valid: true},
		Actor:       "event-actor",
		ActorUserID: pgtype.Int8{Int64: eventUser.ID, Valid: true},
		Action:      "case_viewed",
	})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if !event.ActorUserID.Valid || event.ActorUserID.Int64 != eventUser.ID {
		t.Fatalf("event actor id = %+v", event.ActorUserID)
	}
	if _, err := owner.Exec(ctx, `DELETE FROM users WHERE id = $1`, eventUser.ID); err != nil {
		t.Fatalf("delete event actor: %v", err)
	}
	var actor string
	var actorID pgtype.Int8
	if err := owner.QueryRow(ctx, `SELECT actor, actor_user_id FROM events WHERE id = $1`, event.ID).Scan(&actor, &actorID); err != nil {
		t.Fatalf("reload event: %v", err)
	}
	if actor != "event-actor" || actorID.Valid {
		t.Fatalf("event after actor delete = actor %q id %+v", actor, actorID)
	}

	// A historical event may carry no user id at all.
	legacy, err := q.AppendEvent(ctx, lc1db.AppendEventParams{
		Actor:  "legacy",
		Action: "case_viewed",
	})
	if err != nil {
		t.Fatalf("AppendEvent without actor user: %v", err)
	}
	if legacy.ActorUserID.Valid {
		t.Fatalf("legacy event actor id = %+v, want null", legacy.ActorUserID)
	}
}

func TestAuthLegacyBackfill(t *testing.T) {
	h := newStorageHarness(t)
	migrateTo(t, h, 3)
	owner := h.owner(t)
	ctx := context.Background()

	var caseID int64
	if err := owner.QueryRow(ctx, `
		INSERT INTO cases (source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid)
		VALUES ('legacy', '1.2.3.4.100', '1.2.3.4.101', 'orthanc-legacy', '1.2.3.4.102')
		RETURNING id`).Scan(&caseID); err != nil {
		t.Fatalf("seed legacy case: %v", err)
	}
	var readID int64
	if err := owner.QueryRow(ctx,
		`INSERT INTO reads (case_id, kind, reader) VALUES ($1, 'final', 'dr-legacy') RETURNING id`, caseID).Scan(&readID); err != nil {
		t.Fatalf("seed legacy read: %v", err)
	}

	migrateTo(t, h, -1)

	var legacyID int64
	var display string
	var disabled pgtype.Timestamptz
	var lookup []byte
	if err := owner.QueryRow(ctx,
		`SELECT id, display_name, disabled_at, pin_lookup FROM users ORDER BY id`).Scan(&legacyID, &display, &disabled, &lookup); err != nil {
		t.Fatalf("legacy user: %v", err)
	}
	if display != "Legacy reader" || !disabled.Valid {
		t.Fatalf("legacy user = name %q disabled %+v", display, disabled)
	}
	if len(lookup) != 32 {
		t.Fatalf("legacy pin_lookup length = %d, want 32", len(lookup))
	}
	var count int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("legacy backfill created %d users, want 1", count)
	}
	var ownerID int64
	var readerText string
	if err := owner.QueryRow(ctx, `SELECT reader_user_id, reader FROM reads WHERE id = $1`, readID).Scan(&ownerID, &readerText); err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if ownerID != legacyID {
		t.Fatalf("read owner = %d, want legacy user %d", ownerID, legacyID)
	}
	if readerText != "dr-legacy" {
		t.Fatalf("read reader text = %q, want unchanged dr-legacy", readerText)
	}

	// A post-migration read still writes the caller's reader text and has no
	// owner until the ownership cutover.
	var newReadID int64
	if err := owner.QueryRow(ctx,
		`INSERT INTO reads (case_id, kind, reader) VALUES ($1, 'initial', 'reader') RETURNING id`, caseID).Scan(&newReadID); err != nil {
		t.Fatalf("insert post-migration read: %v", err)
	}
	var newOwner pgtype.Int8
	if err := owner.QueryRow(ctx, `SELECT reader_user_id FROM reads WHERE id = $1`, newReadID).Scan(&newOwner); err != nil {
		t.Fatalf("post-migration read owner: %v", err)
	}
	if newOwner.Valid {
		t.Fatalf("post-migration read owner = %+v, want null", newOwner)
	}

	// An explicit owner must still reference a real user.
	var secondCaseID int64
	if err := owner.QueryRow(ctx, `
		INSERT INTO cases (source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid)
		VALUES ('legacy', '1.2.3.4.103', '1.2.3.4.104', 'orthanc-legacy-2', '1.2.3.4.105')
		RETURNING id`).Scan(&secondCaseID); err != nil {
		t.Fatalf("seed second case: %v", err)
	}
	_, err := owner.Exec(ctx,
		`INSERT INTO reads (case_id, kind, reader, reader_user_id) VALUES ($1, 'initial', 'x', 999999)`, secondCaseID)
	assertSQLState(t, err, "23503", "read owner must exist")

	var applied int64
	if err := owner.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&applied); err != nil {
		t.Fatalf("goose version: %v", err)
	}
	if applied != 4 {
		t.Fatalf("goose version = %d, want 4", applied)
	}
}

func TestAuthNoLegacyUserOnFreshDatabase(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	owner := h.owner(t)
	var count int
	if err := owner.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 0 {
		t.Fatalf("fresh database has %d users, want none", count)
	}
}

func TestAuthMigrationDown(t *testing.T) {
	h := newStorageHarness(t)
	provider := migrateTo(t, h, -1)
	owner := h.owner(t)
	q := lc1db.New(owner)
	ctx := context.Background()

	user, err := q.CreateUser(ctx, lc1db.CreateUserParams{
		DisplayName: "Dr. Rollback",
		PinLookup:   syntheticLookup(0x61),
		PinHash:     "synthetic",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	caseRow := createCase(t, q, "1.2.3.4.110")
	if _, err := q.CreateRead(ctx, lc1db.CreateReadParams{CaseID: caseRow.ID, Kind: "initial", Reader: user.DisplayName}); err != nil {
		t.Fatalf("CreateRead: %v", err)
	}

	// Down only removes the new ownership column and user/session tables; the
	// authoritative reader text column was never dropped.
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("migrate down: %v", err)
	}

	columns := tableColumns(t, owner, "reads")
	if _, ok := columns["reader"]; !ok {
		t.Fatalf("down removed reads.reader")
	}
	if _, ok := columns["reader_user_id"]; ok {
		t.Fatalf("down left reads.reader_user_id")
	}
	events := tableColumns(t, owner, "events")
	if _, ok := events["actor_user_id"]; ok {
		t.Fatalf("down left events.actor_user_id")
	}
	var usersMissing bool
	if err := owner.QueryRow(ctx, `SELECT to_regclass('public.users') IS NULL`).Scan(&usersMissing); err != nil {
		t.Fatalf("to_regclass users: %v", err)
	}
	if !usersMissing {
		t.Fatalf("down left users table")
	}

	// The original identity guard still rejects changing a read's reader text.
	_, err = owner.Exec(ctx, `UPDATE reads SET reader = 'other' WHERE id = (SELECT id FROM reads LIMIT 1)`)
	assertSQLState(t, err, "23514", "identity guard after down")

	// Re-applying the migration returns to the current schema.
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up after down: %v", err)
	}
}

func TestAuthQueryProjections(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	owner := h.owner(t)
	q := lc1db.New(owner)
	ctx := context.Background()

	user, err := q.CreateUser(ctx, lc1db.CreateUserParams{
		DisplayName: "projection-reader",
		PinLookup:   syntheticLookup(0x71),
		PinHash:     "synthetic",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	caseRow := createCase(t, q, "1.2.3.4.120")
	read, err := q.CreateRead(ctx, lc1db.CreateReadParams{CaseID: caseRow.ID, Kind: "initial", Reader: "projection-reader"})
	if err != nil {
		t.Fatalf("CreateRead: %v", err)
	}
	if read.Reader != "projection-reader" || read.ReaderUserID.Valid || read.ReaderDisplayName.Valid {
		t.Fatalf("new read projection = %+v, want reader text and no owner", read)
	}

	// A backfilled row exposes both its owner id and the joined display name
	// while the authoritative reader text stays unchanged.
	if _, err := owner.Exec(ctx,
		`INSERT INTO reads (case_id, kind, reader, reader_user_id) VALUES ($1, 'final', 'legacy-text', $2)`,
		caseRow.ID, user.ID); err != nil {
		t.Fatalf("seed owned read: %v", err)
	}
	owned, err := q.GetRead(ctx, lc1db.GetReadParams{CaseID: caseRow.ID, Kind: "final"})
	if err != nil {
		t.Fatalf("GetRead owned: %v", err)
	}
	if owned.Reader != "legacy-text" || owned.ReaderUserID.Int64 != user.ID || owned.ReaderDisplayName.String != user.DisplayName {
		t.Fatalf("owned read projection = %+v, want owner %d", owned, user.ID)
	}

	_, err = owner.Exec(ctx, `UPDATE reads SET reader = 'other' WHERE id = $1`, read.ID)
	assertSQLState(t, err, "23514", "read identity is immutable")
	locked, err := q.LockRead(ctx, read.ID)
	if err != nil {
		t.Fatalf("LockRead: %v", err)
	}
	if locked.Reader != "projection-reader" || locked.ReaderUserID.Valid {
		t.Fatalf("locked read projection = %+v", locked)
	}

	event, err := q.AppendEvent(ctx, lc1db.AppendEventParams{
		CaseID:      pgtype.Int8{Int64: caseRow.ID, Valid: true},
		Actor:       user.DisplayName,
		ActorUserID: pgtype.Int8{Int64: user.ID, Valid: true},
		Action:      "case_viewed",
	})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if event.ActorUserID.Int64 != user.ID || event.Actor != user.DisplayName {
		t.Fatalf("event projection = %+v", event)
	}

	// Generated read/event models never expose credential material.
	for _, model := range []any{lc1db.Read{}, lc1db.Event{}} {
		value := reflect.TypeOf(model)
		for i := range value.NumField() {
			name := strings.ToLower(value.Field(i).Name)
			if strings.Contains(name, "pin") || strings.Contains(name, "hash") || strings.Contains(name, "digest") {
				t.Fatalf("%s exposes credential field %s", value.Name(), value.Field(i).Name)
			}
		}
	}
}

// newAuthStore provisions the runtime pool the same way the service does and
// returns the PostgreSQL authentication adapter on it.
func newAuthStore(t *testing.T, h *storageHarness) *postgres.AuthStore {
	t.Helper()
	h.migrate(t)
	h.grantRuntime(t)
	pool, err := pgxpool.New(context.Background(), h.runtimeDSN)
	if err != nil {
		t.Fatalf("runtime pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return postgres.NewAuthStore(pool)
}

func TestAuthStoreUsers(t *testing.T) {
	h := newStorageHarness(t)
	store := newAuthStore(t, h)
	ctx := context.Background()

	created, err := store.CreateUser(ctx, "Alice", syntheticLookup(11), "encoded-alice")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.ID <= 0 || created.DisplayName != "Alice" || created.DisabledAt != nil {
		t.Fatalf("created user = %+v", created)
	}

	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].ID != created.ID || users[0].DisplayName != "Alice" {
		t.Fatalf("users = %+v, want the created user", users)
	}

	found, encoded, err := store.FindActiveUserByPINLookup(ctx, syntheticLookup(11))
	if err != nil {
		t.Fatalf("FindActiveUserByPINLookup: %v", err)
	}
	if found.ID != created.ID || encoded != "encoded-alice" {
		t.Fatalf("found = %+v, encoded = %q", found, encoded)
	}
	if _, _, err := store.FindActiveUserByPINLookup(ctx, syntheticLookup(12)); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("missing lookup error = %v, want user not found", err)
	}

	if _, err := store.CreateUser(ctx, "Duplicate", syntheticLookup(11), "other"); !errors.Is(err, auth.ErrDuplicatePINLookup) {
		t.Fatalf("duplicate lookup error = %v, want duplicate sentinel", err)
	}
	users, err = store.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("duplicate create changed the user set: %+v, %v", users, err)
	}
}

func TestAuthStoreSessions(t *testing.T) {
	h := newStorageHarness(t)
	store := newAuthStore(t, h)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "Alice", syntheticLookup(21), "encoded-alice")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	digest := bytes.Repeat([]byte{0x21}, 32)
	if err := store.CreateSession(ctx, digest, user.ID, now.Add(8*time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	session, err := store.FindSession(ctx, digest, now)
	if err != nil {
		t.Fatalf("FindSession: %v", err)
	}
	if session.User.ID != user.ID || session.User.DisplayName != "Alice" {
		t.Fatalf("session = %+v", session)
	}
	if !session.ExpiresAt.Equal(now.Add(8 * time.Hour)) {
		t.Fatalf("session expiry = %v", session.ExpiresAt)
	}
	if _, err := store.FindSession(ctx, digest, now.Add(8*time.Hour)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("expired session error = %v", err)
	}
	if _, err := store.FindSession(ctx, bytes.Repeat([]byte{0x22}, 32), now); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("missing session error = %v", err)
	}

	if err := store.DeleteSession(ctx, digest); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := store.DeleteSession(ctx, digest); err != nil {
		t.Fatalf("repeated DeleteSession: %v", err)
	}
	if _, err := store.FindSession(ctx, digest, now); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("deleted session error = %v", err)
	}

	expiredDigest := bytes.Repeat([]byte{0x23}, 32)
	if err := store.CreateSession(ctx, expiredDigest, user.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("CreateSession expired: %v", err)
	}
	if err := store.DeleteExpiredSessions(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if _, err := store.FindSession(ctx, expiredDigest, now.Add(2*time.Second)); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("pruned session error = %v", err)
	}
}

func TestAuthStoreRotateIsTransactional(t *testing.T) {
	h := newStorageHarness(t)
	store := newAuthStore(t, h)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "Alice", syntheticLookup(31), "encoded-old")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	other, err := store.CreateUser(ctx, "Bob", syntheticLookup(32), "encoded-bob")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	digest := bytes.Repeat([]byte{0x31}, 32)
	if err := store.CreateSession(ctx, digest, user.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := store.RotatePIN(ctx, user.ID, syntheticLookup(33), "encoded-new"); err != nil {
		t.Fatalf("RotatePIN: %v", err)
	}
	if _, _, err := store.FindActiveUserByPINLookup(ctx, syntheticLookup(31)); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("old lookup error = %v", err)
	}
	if _, encoded, err := store.FindActiveUserByPINLookup(ctx, syntheticLookup(33)); err != nil || encoded != "encoded-new" {
		t.Fatalf("new lookup = %q, %v", encoded, err)
	}
	if _, err := store.FindSession(ctx, digest, now); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("old session survived rotation: %v", err)
	}

	// A username collision must roll back the credential update and the session
	// deletion.
	collisionDigest := bytes.Repeat([]byte{0x32}, 32)
	if err := store.CreateSession(ctx, collisionDigest, user.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.RotatePIN(ctx, user.ID, syntheticLookup(32), "encoded-collision"); !errors.Is(err, auth.ErrDuplicatePINLookup) {
		t.Fatalf("collision error = %v", err)
	}
	if _, encoded, err := store.FindActiveUserByPINLookup(ctx, syntheticLookup(33)); err != nil || encoded != "encoded-new" {
		t.Fatalf("collision changed Alice's credential: %q, %v", encoded, err)
	}
	if _, encoded, err := store.FindActiveUserByPINLookup(ctx, syntheticLookup(32)); err != nil || encoded != "encoded-bob" {
		t.Fatalf("collision changed Bob's credential: %q, %v", encoded, err)
	}
	if _, err := store.FindSession(ctx, collisionDigest, now); err != nil {
		t.Fatalf("collision revoked a session: %v", err)
	}

	if err := store.RotatePIN(ctx, other.ID+1000, syntheticLookup(34), "encoded-missing"); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("missing user rotation error = %v", err)
	}
}

func TestAuthStoreRevokeIsTransactionalAndIdempotent(t *testing.T) {
	h := newStorageHarness(t)
	store := newAuthStore(t, h)
	ctx := context.Background()

	user, err := store.CreateUser(ctx, "Alice", syntheticLookup(41), "encoded-alice")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	digest := bytes.Repeat([]byte{0x41}, 32)
	if err := store.CreateSession(ctx, digest, user.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := store.RevokeUser(ctx, user.ID); err != nil {
		t.Fatalf("RevokeUser: %v", err)
	}
	if _, _, err := store.FindActiveUserByPINLookup(ctx, syntheticLookup(41)); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("revoked user still resolves: %v", err)
	}
	if _, err := store.FindSession(ctx, digest, now); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("revocation did not delete the session: %v", err)
	}
	users, err := store.ListUsers(ctx)
	if err != nil || len(users) != 1 || users[0].DisabledAt == nil {
		t.Fatalf("revoked user projection = %+v, %v", users, err)
	}

	if err := store.RevokeUser(ctx, user.ID); err != nil {
		t.Fatalf("repeated RevokeUser: %v", err)
	}
	if err := store.RevokeUser(ctx, user.ID+1000); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("missing user revocation error = %v", err)
	}
}
