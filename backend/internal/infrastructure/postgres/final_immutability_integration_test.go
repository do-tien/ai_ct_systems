package postgres_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"nodule-reader/backend/migrations"
)

const (
	decisionLockedConstraint = "lc1_decision_read_locked"
	caseFinalizedConstraint  = "lc1_case_finalized_immutable"
)

// insertDecisionDirect inserts one candidate decision through direct SQL.
func insertDecisionDirect(t *testing.T, conn *pgx.Conn, readID int64, candidateID, decision string, reason *string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(),
		`INSERT INTO candidate_decisions (read_id, candidate_id, decision, dismiss_reason) VALUES ($1, $2, $3, $4)`,
		readID, candidateID, decision, reason); err != nil {
		t.Fatalf("insert decision: %v", err)
	}
}

// finalizeCaseDirect locks the final read and completes the case through direct
// SQL, mirroring a committed finalize without exercising the adapter.
func finalizeCaseDirect(t *testing.T, conn *pgx.Conn, caseID, finalID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1`, finalID); err != nil {
		t.Fatalf("lock final read: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE cases SET status = 'finalized', finalized_at = now() WHERE id = $1`, caseID); err != nil {
		t.Fatalf("finalize case: %v", err)
	}
}

// TestFinalImmutability proves the LC1-13 guards protect the remaining
// physician-owned records: candidate decisions of a locked read and a finalized
// case, through direct runtime SQL. Editable final decisions still work before
// the lock, and AI job completion remains allowed after finalize.
func TestFinalImmutability(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	owner := h.owner(t)
	runtime := h.runtime(t)

	caseID := seedCaseConn(t, owner, "1.2.3.4.6.6001", "initial_locked")
	initialID := seedReadKind(t, owner, caseID, "initial")
	lockReadDirect(t, owner, initialID)
	finalID := seedReadKind(t, owner, caseID, "final")
	reason := "vessel, not a nodule"

	// Editable final decisions work while the final read is unlocked.
	insertDecisionDirect(t, runtime, finalID, "c1", "accepted", nil)
	insertDecisionDirect(t, runtime, finalID, "c2", "dismissed", &reason)
	if _, err := runtime.Exec(ctx, `UPDATE candidate_decisions SET dismiss_reason = 'updated' WHERE read_id = $1 AND candidate_id = 'c2'`, finalID); err != nil {
		t.Fatalf("unlocked decision update should succeed: %v", err)
	}
	if _, err := runtime.Exec(ctx, `DELETE FROM candidate_decisions WHERE read_id = $1 AND candidate_id = 'c1'`, finalID); err != nil {
		t.Fatalf("unlocked decision delete should succeed: %v", err)
	}

	finalizeCaseDirect(t, runtime, caseID, finalID)

	t.Run("decision mutations on a locked read are rejected", func(t *testing.T) {
		_, err := runtime.Exec(ctx,
			`INSERT INTO candidate_decisions (read_id, candidate_id, decision, dismiss_reason) VALUES ($1, 'c3', 'accepted', NULL)`, finalID)
		assertSQLStateConstraint(t, err, checkViolation, decisionLockedConstraint, "decision insert")

		_, err = runtime.Exec(ctx,
			`UPDATE candidate_decisions SET decision = 'accepted', dismiss_reason = NULL WHERE read_id = $1 AND candidate_id = 'c2'`, finalID)
		assertSQLStateConstraint(t, err, checkViolation, decisionLockedConstraint, "decision update")

		_, err = runtime.Exec(ctx, `DELETE FROM candidate_decisions WHERE read_id = $1`, finalID)
		assertSQLStateConstraint(t, err, checkViolation, decisionLockedConstraint, "decision delete")

		// Moving a decision into a locked parent is rejected.
		_, err = runtime.Exec(ctx, `UPDATE candidate_decisions SET read_id = $1 WHERE read_id = $2`, initialID, finalID)
		assertSQLStateConstraint(t, err, checkViolation, decisionLockedConstraint, "decision reassignment")
	})

	t.Run("a finalized case is immutable", func(t *testing.T) {
		_, err := runtime.Exec(ctx, `UPDATE cases SET status = 'awaiting_read' WHERE id = $1`, caseID)
		assertSQLStateConstraint(t, err, checkViolation, caseFinalizedConstraint, "case reopening")

		_, err = runtime.Exec(ctx, `UPDATE cases SET finalized_at = NULL WHERE id = $1`, caseID)
		assertSQLStateConstraint(t, err, checkViolation, caseFinalizedConstraint, "finalized timestamp edit")

		_, err = runtime.Exec(ctx, `UPDATE cases SET source = 'other' WHERE id = $1`, caseID)
		assertSQLStateConstraint(t, err, checkViolation, caseFinalizedConstraint, "case metadata edit")

		_, err = runtime.Exec(ctx, `DELETE FROM cases WHERE id = $1`, caseID)
		assertSQLStateConstraint(t, err, checkViolation, caseFinalizedConstraint, "case deletion")

		// A true no-op update is permitted.
		if _, err := runtime.Exec(ctx, `UPDATE cases SET status = 'finalized' WHERE id = $1`, caseID); err != nil {
			t.Fatalf("no-op update should succeed: %v", err)
		}
	})

	t.Run("AI job completion remains allowed after finalize", func(t *testing.T) {
		var jobID int64
		if err := runtime.QueryRow(ctx, `INSERT INTO ai_jobs (case_id, status) VALUES ($1, 'queued') RETURNING id`, caseID).Scan(&jobID); err != nil {
			t.Fatalf("insert job: %v", err)
		}
		if _, err := runtime.Exec(ctx, `UPDATE ai_jobs SET status = 'succeeded', finished_at = now() WHERE id = $1`, jobID); err != nil {
			t.Fatalf("terminal job write should succeed after finalize: %v", err)
		}
		if got := countDecisions(t, runtime, finalID); got != 1 {
			t.Fatalf("decisions = %d, want 1 untouched", got)
		}
	})
}

// TestDecisionWriteSerializesWithFinalLock proves that a decision write on an
// unlocked read acquires the parent read lock, so a concurrent final lock waits
// for the decision transaction to finish.
func TestDecisionWriteSerializesWithFinalLock(t *testing.T) {
	ctx := context.Background()
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)

	owner := h.owner(t)
	caseID := seedCaseConn(t, owner, "1.2.3.4.6.6002", "initial_locked")
	initialID := seedReadKind(t, owner, caseID, "initial")
	lockReadDirect(t, owner, initialID)
	finalID := seedReadKind(t, owner, caseID, "final")

	writer := h.runtime(t)
	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO candidate_decisions (read_id, candidate_id, decision) VALUES ($1, 'c1', 'accepted')`, finalID); err != nil {
		t.Fatalf("writer insert: %v", err)
	}

	locker := h.runtime(t)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := locker.Exec(ctx, `UPDATE reads SET locked_at = now() WHERE id = $1`, finalID)
		done <- err
	}()
	<-started

	select {
	case err := <-done:
		t.Fatalf("final lock completed while the decision transaction held the parent lock: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit writer: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("final lock failed after the writer committed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("final lock did not proceed after the writer committed")
	}
}

// TestLC1FinalImmutabilityMigration proves migration 00003 can be applied,
// re-applied and reversed without losing finalized records, and that its guards
// are active again after a re-upgrade.
func TestLC1FinalImmutabilityMigration(t *testing.T) {
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

	if _, err := provider.UpTo(ctx, 2); err != nil {
		t.Fatalf("up to 2: %v", err)
	}

	owner := h.owner(t)
	caseID := seedCaseConn(t, owner, "1.2.3.4.6.6003", "initial_locked")
	initialID := seedReadKind(t, owner, caseID, "initial")
	lockReadDirect(t, owner, initialID)
	finalID := seedReadKind(t, owner, caseID, "final")
	insertDecisionDirect(t, owner, finalID, "c1", "accepted", nil)
	finalizeCaseDirect(t, owner, caseID, finalID)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("upgrade to 3: %v", err)
	}
	_, err = owner.Exec(ctx, `DELETE FROM candidate_decisions WHERE read_id = $1`, finalID)
	assertSQLStateConstraint(t, err, checkViolation, decisionLockedConstraint, "guard active after upgrade")
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("repeated up: %v", err)
	}

	if _, err := provider.DownTo(ctx, 2); err != nil {
		t.Fatalf("downgrade to 2: %v", err)
	}
	var guardFunctions int
	if err := owner.QueryRow(ctx,
		`SELECT count(*) FROM pg_proc WHERE proname IN ('lc1_guard_decision_mutation', 'lc1_guard_finalized_case')`).Scan(&guardFunctions); err != nil {
		t.Fatalf("count guard functions: %v", err)
	}
	if guardFunctions != 0 {
		t.Fatalf("down left %d guard functions", guardFunctions)
	}
	// With the guard removed, the previously frozen records can be changed.
	if _, err := owner.Exec(ctx, `UPDATE candidate_decisions SET decision = 'accepted', dismiss_reason = NULL WHERE read_id = $1`, finalID); err != nil {
		t.Fatalf("down should remove the decision guard: %v", err)
	}
	if _, err := owner.Exec(ctx, `UPDATE cases SET status = 'awaiting_read' WHERE id = $1`, caseID); err != nil {
		t.Fatalf("down should remove the case guard: %v", err)
	}
	// The finalized record survived the round trip.
	if got := countDecisions(t, owner, finalID); got != 1 {
		t.Fatalf("decisions after down = %d, want 1", got)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-upgrade to 3: %v", err)
	}
	// Prove the guards are active again with a fresh finalized case.
	caseID2 := seedCaseConn(t, owner, "1.2.3.4.6.6004", "initial_locked")
	initialID2 := seedReadKind(t, owner, caseID2, "initial")
	lockReadDirect(t, owner, initialID2)
	finalID2 := seedReadKind(t, owner, caseID2, "final")
	finalizeCaseDirect(t, owner, caseID2, finalID2)

	_, err = owner.Exec(ctx, `UPDATE cases SET status = 'awaiting_read' WHERE id = $1`, caseID2)
	assertSQLStateConstraint(t, err, checkViolation, caseFinalizedConstraint, "guard re-active after re-upgrade")
	_, err = owner.Exec(ctx,
		`INSERT INTO candidate_decisions (read_id, candidate_id, decision) VALUES ($1, 'c9', 'accepted')`, finalID2)
	assertSQLStateConstraint(t, err, checkViolation, decisionLockedConstraint, "decision guard re-active after re-upgrade")
}
