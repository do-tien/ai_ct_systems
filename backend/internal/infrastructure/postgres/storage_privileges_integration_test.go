package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"nodule-reader/backend/internal/infrastructure/postgres/lc1db"
)

func TestLC1StoragePrivileges(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	h.grantRuntime(t)
	owner := h.owner(t)
	runtime := h.runtime(t)
	ctx := context.Background()
	oq := lc1db.New(owner)
	rq := lc1db.New(runtime)

	t.Run("runtime_role_is_not_owner_or_superuser", func(t *testing.T) {
		var super, createdb, createrole bool
		if err := runtime.QueryRow(ctx,
			`SELECT rolsuper, rolcreatedb, rolcreaterole FROM pg_roles WHERE rolname = current_user`).
			Scan(&super, &createdb, &createrole); err != nil {
			t.Fatalf("role attributes: %v", err)
		}
		if super || createdb || createrole {
			t.Fatalf("runtime attributes: super=%v createdb=%v createrole=%v", super, createdb, createrole)
		}
		var memberOfOwner bool
		if err := runtime.QueryRow(ctx, `SELECT pg_has_role(current_user, $1, 'MEMBER')`, h.ownerRole).Scan(&memberOfOwner); err != nil {
			t.Fatalf("membership: %v", err)
		}
		if memberOfOwner {
			t.Fatalf("runtime is a member of the owner role")
		}
		var ownedTables int
		if err := runtime.QueryRow(ctx,
			`SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = current_user`).Scan(&ownedTables); err != nil {
			t.Fatalf("owned tables: %v", err)
		}
		if ownedTables != 0 {
			t.Fatalf("runtime owns %d tables", ownedTables)
		}
	})

	caseRow := createCase(t, oq, "1.2.3.4.40")
	initialRead := createRead(t, oq, caseRow.ID, "initial")
	createMark(t, oq, initialRead.ID, "reader", "", "seed")

	t.Run("runtime_ordinary_crud", func(t *testing.T) {
		got, err := rq.GetCase(ctx, caseRow.ID)
		if err != nil {
			t.Fatalf("runtime GetCase: %v", err)
		}
		if got.SeriesInstanceUid != "1.2.3.4.40" {
			t.Fatalf("case = %+v", got)
		}
		if _, err := rq.UpdateCaseState(ctx, lc1db.UpdateCaseStateParams{ID: caseRow.ID, Status: "initial_locked"}); err != nil {
			t.Fatalf("runtime UpdateCaseState: %v", err)
		}
		if _, err := rq.LockRead(ctx, initialRead.ID); err != nil {
			t.Fatalf("runtime LockRead: %v", err)
		}
		newRead := createRead(t, rq, caseRow.ID, "final")
		if err := rq.CopyMarks(ctx, lc1db.CopyMarksParams{SourceReadID: initialRead.ID, TargetReadID: newRead.ID}); err != nil {
			t.Fatalf("runtime CopyMarks: %v", err)
		}
		if _, err := rq.UpsertCandidateDecision(ctx, lc1db.UpsertCandidateDecisionParams{
			ReadID: newRead.ID, CandidateID: "c1", Decision: "accepted",
		}); err != nil {
			t.Fatalf("runtime decision: %v", err)
		}
		if _, err := rq.CreateAIJob(ctx, caseRow.ID); err != nil {
			t.Fatalf("runtime CreateAIJob: %v", err)
		}
	})

	t.Run("runtime_event_insert_and_read", func(t *testing.T) {
		appended, err := rq.AppendEvent(ctx, lc1db.AppendEventParams{
			Actor: "dr-synthetic", Action: "case_viewed",
			Detail: []byte(`{"synthetic": true}`),
			CaseID: pgtype.Int8{Int64: caseRow.ID, Valid: true},
		})
		if err != nil {
			t.Fatalf("runtime AppendEvent: %v", err)
		}
		events, err := rq.ListEvents(ctx, pgtype.Int8{Int64: caseRow.ID, Valid: true})
		if err != nil {
			t.Fatalf("runtime ListEvents: %v", err)
		}
		found := false
		for _, event := range events {
			if event.ID == appended.ID && strings.Contains(string(event.Detail), "synthetic") {
				found = true
			}
		}
		if !found {
			t.Fatalf("appended event %d not readable", appended.ID)
		}
	})

	t.Run("runtime_event_mutations_denied", func(t *testing.T) {
		var eventID int64
		if err := runtime.QueryRow(ctx, `SELECT id FROM events ORDER BY id LIMIT 1`).Scan(&eventID); err != nil {
			t.Fatalf("event id: %v", err)
		}
		denied := []struct {
			label string
			stmt  string
			args  []any
		}{
			{"update_events", `UPDATE events SET action = 'tampered' WHERE id = $1`, []any{eventID}},
			{"delete_events", `DELETE FROM events WHERE id = $1`, []any{eventID}},
			{"truncate_events", `TRUNCATE events`, nil},
			{"create_table", `CREATE TABLE lc1_07_should_not_exist (id int)`, nil},
			{"alter_events", `ALTER TABLE events ADD COLUMN hacked text`, nil},
			{"set_role_owner", `SET ROLE ` + quoteIdent(h.ownerRole), nil},
		}
		for _, tc := range denied {
			t.Run(tc.label, func(t *testing.T) {
				_, err := runtime.Exec(ctx, tc.stmt, tc.args...)
				assertSQLState(t, err, "42501", tc.label)
			})
		}

		// Same denials on a fresh runtime connection (privilege is a real boundary).
		reconnected := h.runtime(t)
		if _, err := reconnected.Exec(ctx, `DELETE FROM events WHERE id = $1`, eventID); err == nil {
			t.Fatalf("event DELETE succeeded after reconnect")
		} else {
			assertSQLState(t, err, "42501", "delete after reconnect")
		}
		// The event still exists untouched.
		var action string
		if err := runtime.QueryRow(ctx, `SELECT action FROM events WHERE id = $1`, eventID).Scan(&action); err != nil {
			t.Fatalf("event survived read: %v", err)
		}
		if action == "tampered" {
			t.Fatalf("event was mutated")
		}
	})

	t.Run("event_and_business_write_roll_back_together", func(t *testing.T) {
		tx, err := runtime.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		txq := lc1db.New(tx)
		if _, err := txq.AppendEvent(ctx, lc1db.AppendEventParams{
			Actor: "dr-synthetic", Action: "rolled_back", CaseID: pgtype.Int8{Int64: caseRow.ID, Valid: true},
		}); err != nil {
			t.Fatalf("append event: %v", err)
		}
		if _, err := txq.UpdateCaseState(ctx, lc1db.UpdateCaseStateParams{ID: caseRow.ID, Status: "finalized"}); err != nil {
			t.Fatalf("update case: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}

		var rolledBackEvents int
		if err := runtime.QueryRow(ctx, `SELECT count(*) FROM events WHERE action = 'rolled_back'`).Scan(&rolledBackEvents); err != nil {
			t.Fatalf("count events: %v", err)
		}
		if rolledBackEvents != 0 {
			t.Fatalf("rolled-back event persisted")
		}
		got, err := rq.GetCase(ctx, caseRow.ID)
		if err != nil {
			t.Fatalf("GetCase: %v", err)
		}
		if got.Status == "finalized" {
			t.Fatalf("rolled-back case status persisted")
		}
	})

	t.Run("audited_case_deletion_is_restricted", func(t *testing.T) {
		// The audited case above has a locked initial read: the immutability
		// guard denies deleting its marks/read even for the owner.
		_, err := owner.Exec(ctx, `DELETE FROM marks WHERE read_id = $1`, initialRead.ID)
		assertSQLStateConstraint(t, err, checkViolation, markLockedConstraint, "delete marks of locked read")
		_, err = owner.Exec(ctx, `DELETE FROM reads WHERE id = $1`, initialRead.ID)
		assertSQLStateConstraint(t, err, checkViolation, readImmutableConstraint, "delete locked read")
		if got := countMarksOn(t, owner, initialRead.ID); got != 1 {
			t.Fatalf("locked marks = %d, want 1 still present", got)
		}

		// A separate unlocked fixture exercises the audit-trail FK path.
		deletionCase := createCase(t, oq, "1.2.3.4.41")
		deletionRead := createRead(t, oq, deletionCase.ID, "initial")
		createMark(t, oq, deletionRead.ID, "reader", "", "deletable")
		if _, err := oq.AppendEvent(ctx, lc1db.AppendEventParams{
			Actor: "dr-synthetic", Action: "case_viewed",
			CaseID: pgtype.Int8{Int64: deletionCase.ID, Valid: true},
		}); err != nil {
			t.Fatalf("append deletion event: %v", err)
		}
		for _, statement := range []string{
			`DELETE FROM marks WHERE read_id IN (SELECT id FROM reads WHERE case_id = $1)`,
			`DELETE FROM reads WHERE case_id = $1`,
			`DELETE FROM ai_jobs WHERE case_id = $1`,
		} {
			if _, err := owner.Exec(ctx, statement, deletionCase.ID); err != nil {
				t.Fatalf("cleanup statement failed: %v", err)
			}
		}
		_, err = owner.Exec(ctx, `DELETE FROM cases WHERE id = $1`, deletionCase.ID)
		assertSQLState(t, err, "23503", "delete audited case")

		// Once the audit trail is removed by the owner, the case can go.
		if _, err := owner.Exec(ctx, `DELETE FROM events WHERE case_id = $1`, deletionCase.ID); err != nil {
			t.Fatalf("delete events: %v", err)
		}
		if _, err := owner.Exec(ctx, `DELETE FROM cases WHERE id = $1`, deletionCase.ID); err != nil {
			t.Fatalf("delete case after clearing audit trail: %v", err)
		}
	})
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
