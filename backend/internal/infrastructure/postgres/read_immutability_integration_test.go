package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"nodule-reader/backend/internal/application/initialread"
	"nodule-reader/backend/migrations"
)

const (
	readImmutableConstraint = "lc1_read_immutable"
	markLockedConstraint    = "lc1_mark_read_locked"
	checkViolation          = "23514"
)

func assertSQLStateConstraint(t *testing.T, err error, code, constraint, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: statement unexpectedly succeeded, want %s/%s", label, code, constraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: error = %v, want *pgconn.PgError", label, err)
	}
	if pgErr.Code != code {
		t.Fatalf("%s: SQLSTATE = %s (%s), want %s", label, pgErr.Code, pgErr.Message, code)
	}
	if pgErr.ConstraintName != constraint {
		t.Fatalf("%s: constraint = %q, want %q", label, pgErr.ConstraintName, constraint)
	}
}

// seedCaseConn inserts one case directly on a connection.
func seedCaseConn(t *testing.T, conn *pgx.Conn, seriesUID, status string) int64 {
	t.Helper()
	var caseID int64
	if err := conn.QueryRow(context.Background(),
		`INSERT INTO cases (source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid, status)
		 VALUES ('LC1-11/SYNTHETIC', '1.2.3.4.5.99', $1, 'orthanc-1', '1.2.3.4.7', $2) RETURNING id`,
		seriesUID, status).Scan(&caseID); err != nil {
		t.Fatalf("seed case: %v", err)
	}
	return caseID
}

// seedReadKind inserts one read of the given kind directly on a connection.
func seedReadKind(t *testing.T, conn *pgx.Conn, caseID int64, kind string) int64 {
	t.Helper()
	var readID int64
	if err := conn.QueryRow(context.Background(),
		`INSERT INTO reads (case_id, kind, reader) VALUES ($1, $2, $3) RETURNING id`,
		caseID, kind, initialread.Reader).Scan(&readID); err != nil {
		t.Fatalf("seed %s read: %v", kind, err)
	}
	return readID
}

// lockReadDirect locks a read the way a runtime writer would.
func lockReadDirect(t *testing.T, conn *pgx.Conn, readID int64) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), `UPDATE reads SET locked_at = now() WHERE id = $1`, readID); err != nil {
		t.Fatalf("lock read %d: %v", readID, err)
	}
}

func insertMarkDirect(t *testing.T, conn *pgx.Conn, readID int64, note string) int64 {
	t.Helper()
	var markID int64
	if err := conn.QueryRow(context.Background(),
		`INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, view_plane_normal, note)
		 VALUES ($1, '{-1,0,0}'::float8[], '{1,0,0}'::float8[], '{0,0,0}'::float8[], 2, 'reader', '{0,0,1}'::float8[], $2)
		 RETURNING id`, readID, note).Scan(&markID); err != nil {
		t.Fatalf("insert mark into read %d: %v", readID, err)
	}
	return markID
}

func countMarksOn(t *testing.T, conn *pgx.Conn, readID int64) int {
	t.Helper()
	var count int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM marks WHERE read_id = $1`, readID).Scan(&count); err != nil {
		t.Fatalf("mark count: %v", err)
	}
	return count
}

func readLockedAt(t *testing.T, conn *pgx.Conn, readID int64) *time.Time {
	t.Helper()
	var locked *time.Time
	if err := conn.QueryRow(context.Background(), `SELECT locked_at FROM reads WHERE id = $1`, readID).Scan(&locked); err != nil {
		t.Fatalf("read locked_at: %v", err)
	}
	return locked
}

func TestReadImmutability(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	owner := h.owner(t)
	runtime := h.runtime(t)

	caseID := seedCaseConn(t, runtime, "1.2.3.4.6.100", "initial_locked")
	lockedRead := seedReadKind(t, runtime, caseID, "initial")
	lockedMark := insertMarkDirect(t, runtime, lockedRead, "locked")
	lockReadDirect(t, runtime, lockedRead)

	emptyCase := seedCaseConn(t, runtime, "1.2.3.4.6.101", "initial_locked")
	emptyRead := seedReadKind(t, runtime, emptyCase, "initial")
	lockReadDirect(t, runtime, emptyRead)

	unlockedCase := seedCaseConn(t, runtime, "1.2.3.4.6.102", "awaiting_read")
	unlockedRead := seedReadKind(t, runtime, unlockedCase, "initial")

	denied := []struct {
		label string
		stmt  string
		args  []any
	}{
		{"clear_locked_at", `UPDATE reads SET locked_at = NULL WHERE id = $1`, []any{lockedRead}},
		{"replace_locked_at", `UPDATE reads SET locked_at = now() WHERE id = $1`, []any{lockedRead}},
		{"change_reader", `UPDATE reads SET reader = 'other' WHERE id = $1`, []any{lockedRead}},
		{"change_kind", `UPDATE reads SET kind = 'final' WHERE id = $1`, []any{lockedRead}},
		{"change_id", `UPDATE reads SET id = id + 1000 WHERE id = $1`, []any{lockedRead}},
		{"change_created_at", `UPDATE reads SET created_at = now() WHERE id = $1`, []any{lockedRead}},
		{"change_case_id", `UPDATE reads SET case_id = $2 WHERE id = $1`, []any{lockedRead, emptyCase}},
		{"delete_locked_read", `DELETE FROM reads WHERE id = $1`, []any{lockedRead}},
		{"delete_empty_locked_read", `DELETE FROM reads WHERE id = $1`, []any{emptyRead}},
	}
	for _, tc := range denied {
		t.Run("owner_denied/"+tc.label, func(t *testing.T) {
			_, err := owner.Exec(ctx, tc.stmt, tc.args...)
			assertSQLStateConstraint(t, err, checkViolation, readImmutableConstraint, tc.label)
		})
	}

	unlockedIdentity := []struct {
		label string
		stmt  string
	}{
		{"change_reader", `UPDATE reads SET reader = 'other' WHERE id = $1`},
		{"change_kind", `UPDATE reads SET kind = 'final' WHERE id = $1`},
		{"change_id", `UPDATE reads SET id = id + 1000 WHERE id = $1`},
		{"change_case_id", `UPDATE reads SET case_id = case_id + 1 WHERE id = $1`},
		{"change_created_at", `UPDATE reads SET created_at = now() WHERE id = $1`},
	}
	for _, tc := range unlockedIdentity {
		t.Run("unlocked_identity_denied/"+tc.label, func(t *testing.T) {
			_, err := owner.Exec(ctx, tc.stmt, unlockedRead)
			assertSQLStateConstraint(t, err, checkViolation, readImmutableConstraint, tc.label)
		})
	}

	// A first NULL->non-NULL lock on an unlocked read is allowed.
	lockReadDirect(t, runtime, unlockedRead)
	if readLockedAt(t, owner, unlockedRead) == nil {
		t.Fatalf("unlocked read was not lockable")
	}

	// The locked snapshot is unchanged after every denied attempt.
	if _, err := owner.Exec(ctx, `UPDATE cases SET status = 'awaiting_read' WHERE id = $1`, caseID); err != nil {
		t.Fatalf("case state reset: %v", err)
	}
	var (
		reader string
		locked *time.Time
	)
	if err := owner.QueryRow(ctx, `SELECT reader, locked_at FROM reads WHERE id = $1`, lockedRead).Scan(&reader, &locked); err != nil {
		t.Fatalf("locked read: %v", err)
	}
	if reader != initialread.Reader || locked == nil {
		t.Fatalf("locked read mutated: reader=%q locked=%v", reader, locked)
	}
	if got := countMarksOn(t, owner, lockedRead); got != 1 {
		t.Fatalf("locked marks = %d, want 1", got)
	}
	var survivingID int64
	if err := owner.QueryRow(ctx, `SELECT id FROM marks WHERE read_id = $1`, lockedRead).Scan(&survivingID); err != nil {
		t.Fatalf("surviving mark: %v", err)
	}
	if survivingID != lockedMark {
		t.Fatalf("mark id = %d, want %d", survivingID, lockedMark)
	}

	// A case-state reset does not permit unlocking or editing locked marks.
	_, err := owner.Exec(ctx, `UPDATE reads SET locked_at = NULL WHERE id = $1`, lockedRead)
	assertSQLStateConstraint(t, err, checkViolation, readImmutableConstraint, "unlock after case-state reset")
	_, err = owner.Exec(ctx, `UPDATE marks SET diameter_mm = 99 WHERE read_id = $1`, lockedRead)
	assertSQLStateConstraint(t, err, checkViolation, markLockedConstraint, "mark edit after case-state reset")
}

func TestLockedMarkMutations(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	owner := h.owner(t)
	runtime := h.runtime(t)

	caseID := seedCaseConn(t, runtime, "1.2.3.4.6.200", "initial_locked")
	initialRead := seedReadKind(t, runtime, caseID, "initial")
	initialMark := insertMarkDirect(t, runtime, initialRead, "initial")
	lockReadDirect(t, runtime, initialRead)

	finalRead := seedReadKind(t, runtime, caseID, "final")
	finalMark := insertMarkDirect(t, runtime, finalRead, "final")

	lockedAttempts := []struct {
		label string
		stmt  string
		args  []any
	}{
		{"insert_into_locked", `INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, view_plane_normal)
		 VALUES ($1, '{-2,0,0}'::float8[], '{2,0,0}'::float8[], '{0,0,0}'::float8[], 4, 'reader', '{0,0,1}'::float8[])`, []any{initialRead}},
		{"update_geometry_in_locked", `UPDATE marks SET diameter_mm = 99 WHERE read_id = $1`, []any{initialRead}},
		{"update_note_in_locked", `UPDATE marks SET note = 'tampered' WHERE read_id = $1`, []any{initialRead}},
		{"delete_from_locked", `DELETE FROM marks WHERE read_id = $1`, []any{initialRead}},
		{"move_locked_to_unlocked", `UPDATE marks SET read_id = $2 WHERE read_id = $1`, []any{initialRead, finalRead}},
		{"move_unlocked_into_locked", `UPDATE marks SET read_id = $2 WHERE read_id = $1`, []any{finalRead, initialRead}},
	}
	for _, tc := range lockedAttempts {
		t.Run(tc.label, func(t *testing.T) {
			_, err := runtime.Exec(ctx, tc.stmt, tc.args...)
			assertSQLStateConstraint(t, err, checkViolation, markLockedConstraint, tc.label)
		})
	}

	// Both parent lists are unchanged.
	if got := countMarksOn(t, owner, initialRead); got != 1 {
		t.Fatalf("initial marks = %d, want 1", got)
	}
	if got := countMarksOn(t, owner, finalRead); got != 1 {
		t.Fatalf("final marks = %d, want 1", got)
	}
	var note string
	var diameter float64
	if err := owner.QueryRow(ctx, `SELECT note, diameter_mm FROM marks WHERE id = $1`, initialMark).Scan(&note, &diameter); err != nil {
		t.Fatalf("initial mark: %v", err)
	}
	if note != "initial" || diameter != 2 {
		t.Fatalf("initial mark mutated: note=%q diameter=%v", note, diameter)
	}

	// The unlocked final remains editable.
	if _, err := runtime.Exec(ctx, `UPDATE marks SET diameter_mm = 7.5, note = 'edited' WHERE id = $1`, finalMark); err != nil {
		t.Fatalf("final mark update: %v", err)
	}
	extra := insertMarkDirect(t, runtime, finalRead, "extra")
	if _, err := runtime.Exec(ctx, `DELETE FROM marks WHERE id = $1`, extra); err != nil {
		t.Fatalf("final mark delete: %v", err)
	}
	if err := owner.QueryRow(ctx, `SELECT note, diameter_mm FROM marks WHERE id = $1`, finalMark).Scan(&note, &diameter); err != nil {
		t.Fatalf("final mark after edit: %v", err)
	}
	if note != "edited" || diameter != 7.5 {
		t.Fatalf("final mark edit not applied: note=%q diameter=%v", note, diameter)
	}
	if got := countMarksOn(t, owner, finalRead); got != 1 {
		t.Fatalf("final marks = %d, want 1", got)
	}
}

// TestReadLockSerializesDirectMarkWrite proves the mark trigger takes a parent
// read row lock, so a mark writer and a lock update cannot both commit based on
// stale eligibility.
func TestReadLockSerializesDirectMarkWrite(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)

	t.Run("writer first makes the lock wait", func(t *testing.T) {
		runtime1 := h.runtime(t)
		runtime2 := h.runtime(t)
		caseID := seedCaseConn(t, runtime1, "1.2.3.4.6.300", "awaiting_read")
		readID := seedReadKind(t, runtime1, caseID, "initial")

		writer, err := runtime1.Begin(ctx)
		if err != nil {
			t.Fatalf("begin writer: %v", err)
		}
		defer writer.Rollback(ctx)
		if _, err := writer.Exec(ctx,
			`INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, view_plane_normal)
			 VALUES ($1, '{-1,0,0}'::float8[], '{1,0,0}'::float8[], '{0,0,0}'::float8[], 2, 'reader', '{0,0,1}'::float8[])`,
			readID); err != nil {
			t.Fatalf("writer insert: %v", err)
		}

		locker, err := runtime2.Begin(ctx)
		if err != nil {
			t.Fatalf("begin locker: %v", err)
		}
		defer locker.Rollback(ctx)

		type result struct {
			locked *time.Time
			err    error
		}
		done := make(chan result, 1)
		go func() {
			var locked *time.Time
			err := locker.QueryRow(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1 AND locked_at IS NULL RETURNING locked_at`, readID).Scan(&locked)
			done <- result{locked: locked, err: err}
		}()

		select {
		case got := <-done:
			t.Fatalf("lock did not wait for the mark writer: %+v", got)
		case <-time.After(300 * time.Millisecond):
		}

		if err := writer.Commit(ctx); err != nil {
			t.Fatalf("writer commit: %v", err)
		}
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("lock after writer commit: %v", got.err)
			}
			if got.locked == nil {
				t.Fatalf("lock returned no timestamp")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("lock never completed after the writer committed")
		}
		// The lock transaction now observes the committed mark.
		var marks int
		if err := locker.QueryRow(ctx, `SELECT count(*) FROM marks WHERE read_id = $1`, readID).Scan(&marks); err != nil {
			t.Fatalf("locker mark count: %v", err)
		}
		if marks != 1 {
			t.Fatalf("locker sees %d marks, want the committed 1", marks)
		}
	})

	t.Run("lock first rejects the writer", func(t *testing.T) {
		runtime1 := h.runtime(t)
		runtime2 := h.runtime(t)
		caseID := seedCaseConn(t, runtime1, "1.2.3.4.6.301", "awaiting_read")
		readID := seedReadKind(t, runtime1, caseID, "initial")

		locker, err := runtime2.Begin(ctx)
		if err != nil {
			t.Fatalf("begin locker: %v", err)
		}
		defer locker.Rollback(ctx)
		if _, err := locker.Exec(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1`, readID); err != nil {
			t.Fatalf("locker update: %v", err)
		}

		writer, err := runtime1.Begin(ctx)
		if err != nil {
			t.Fatalf("begin writer: %v", err)
		}
		defer writer.Rollback(ctx)

		done := make(chan error, 1)
		go func() {
			_, err := writer.Exec(ctx,
				`INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, view_plane_normal)
				 VALUES ($1, '{-1,0,0}'::float8[], '{1,0,0}'::float8[], '{0,0,0}'::float8[], 2, 'reader', '{0,0,1}'::float8[])`,
				readID)
			done <- err
		}()

		select {
		case err := <-done:
			t.Fatalf("writer did not wait for the lock: err=%v", err)
		case <-time.After(300 * time.Millisecond):
		}

		if err := locker.Commit(ctx); err != nil {
			t.Fatalf("locker commit: %v", err)
		}
		select {
		case err := <-done:
			assertSQLStateConstraint(t, err, checkViolation, markLockedConstraint, "writer after lock commit")
		case <-time.After(5 * time.Second):
			t.Fatalf("writer never completed after the lock committed")
		}
	})
}

func TestReadImmutabilityRuntimePrivileges(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	runtime := h.runtime(t)
	owner := h.owner(t)

	caseID := seedCaseConn(t, runtime, "1.2.3.4.6.400", "initial_locked")
	readID := seedReadKind(t, runtime, caseID, "initial")
	insertMarkDirect(t, runtime, readID, "locked")
	lockReadDirect(t, runtime, readID)

	deniedPrivilege := []struct {
		label string
		stmt  string
	}{
		{"truncate_reads", `TRUNCATE reads`},
		{"truncate_marks", `TRUNCATE marks`},
		{"disable_trigger", `ALTER TABLE reads DISABLE TRIGGER lc1_reads_guard`},
		{"disable_mark_trigger", `ALTER TABLE marks DISABLE TRIGGER lc1_marks_guard`},
		{"set_session_replication_role", `SET session_replication_role = replica`},
		{"set_role_owner", `SET ROLE ` + quoteIdent(h.ownerRole)},
		{"alter_reads", `ALTER TABLE reads ADD COLUMN hacked text`},
		{"drop_trigger", `DROP TRIGGER lc1_reads_guard ON reads`},
	}
	for _, tc := range deniedPrivilege {
		t.Run(tc.label, func(t *testing.T) {
			_, err := runtime.Exec(ctx, tc.stmt)
			assertSQLState(t, err, "42501", tc.label)
		})
	}

	// Reconnect: the boundary is real, not session state; the locked snapshot is
	// identical after every denied attempt.
	reconnected := h.runtime(t)
	_, err := reconnected.Exec(ctx, `UPDATE reads SET locked_at = NULL WHERE id = $1`, readID)
	assertSQLStateConstraint(t, err, checkViolation, readImmutableConstraint, "unlock after reconnect")
	_, err = reconnected.Exec(ctx, `DELETE FROM marks WHERE read_id = $1`, readID)
	assertSQLStateConstraint(t, err, checkViolation, markLockedConstraint, "mark delete after reconnect")
	_, err = reconnected.Exec(ctx, `UPDATE reads SET id = id + 1000 WHERE id = $1`, readID)
	assertSQLStateConstraint(t, err, checkViolation, readImmutableConstraint, "reidentify after reconnect")

	if readLockedAt(t, owner, readID) == nil {
		t.Fatalf("locked read was unlocked")
	}
	if got := countMarksOn(t, owner, readID); got != 1 {
		t.Fatalf("locked marks = %d, want 1", got)
	}
}

func TestLC1ReadImmutabilityMigration(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	db, err := sql.Open("pgx", h.ownerDSN)
	if err != nil {
		t.Fatalf("open owner handle: %v", err)
	}
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.Files)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	defer provider.Close()

	if _, err := provider.UpTo(ctx, 1); err != nil {
		t.Fatalf("up to 1: %v", err)
	}
	owner := h.owner(t)
	caseID := seedCaseConn(t, owner, "1.2.3.4.6.500", "awaiting_read")
	readID := seedReadKind(t, owner, caseID, "initial")
	markID := insertMarkDirect(t, owner, readID, "survivor")
	var eventID int64
	if err := owner.QueryRow(ctx,
		`INSERT INTO events (case_id, actor, action) VALUES ($1, 'reader', 'seeded') RETURNING id`, caseID).Scan(&eventID); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("upgrade to 2: %v", err)
	}
	lockReadDirect(t, owner, readID)
	_, err = owner.Exec(ctx, `DELETE FROM marks WHERE read_id = $1`, readID)
	assertSQLStateConstraint(t, err, checkViolation, markLockedConstraint, "guard active after upgrade")
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("repeated up: %v", err)
	}

	if _, err := provider.DownTo(ctx, 1); err != nil {
		t.Fatalf("downgrade to 1: %v", err)
	}
	var guardFunctions int
	if err := owner.QueryRow(ctx,
		`SELECT count(*) FROM pg_proc WHERE proname IN ('lc1_guard_read_mutation', 'lc1_guard_mark_mutation')`).Scan(&guardFunctions); err != nil {
		t.Fatalf("count guard functions: %v", err)
	}
	if guardFunctions != 0 {
		t.Fatalf("down left %d guard functions", guardFunctions)
	}
	if _, err := owner.Exec(ctx, `UPDATE reads SET locked_at = NULL WHERE id = $1`, readID); err != nil {
		t.Fatalf("down should remove the read guard: %v", err)
	}

	// Data survives both directions.
	if got := countMarksOn(t, owner, readID); got != 1 {
		t.Fatalf("marks after down = %d, want 1", got)
	}
	var survivingMark int64
	if err := owner.QueryRow(ctx, `SELECT id FROM marks WHERE read_id = $1`, readID).Scan(&survivingMark); err != nil {
		t.Fatalf("surviving mark: %v", err)
	}
	if survivingMark != markID {
		t.Fatalf("mark id = %d, want %d", survivingMark, markID)
	}
	var survivingEvent int64
	if err := owner.QueryRow(ctx, `SELECT id FROM events WHERE id = $1`, eventID).Scan(&survivingEvent); err != nil {
		t.Fatalf("surviving event: %v", err)
	}
	if survivingEvent != eventID {
		t.Fatalf("event id = %d, want %d", survivingEvent, eventID)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-upgrade to 2: %v", err)
	}
	lockReadDirect(t, owner, readID)
	_, err = owner.Exec(ctx, `DELETE FROM marks WHERE read_id = $1`, readID)
	assertSQLStateConstraint(t, err, checkViolation, markLockedConstraint, "guard re-active after re-upgrade")
}
