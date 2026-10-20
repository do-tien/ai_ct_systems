package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"nodule-reader/backend/migrations"
)

const adminDSNEnv = "LC1_STORAGE_TEST_ADMIN_DSN"

// storageHarness provisions a disposable database with separate owner and
// runtime logins on a loopback server. Every database, role and password is
// generated per test and removed afterwards; nothing is derived from the
// application's POSTGRES_* settings.
type storageHarness struct {
	ownerDSN    string
	runtimeDSN  string
	ownerRole   string
	runtimeRole string
	database    string
	admin       *pgx.Conn
}

func randomBytes(t *testing.T, size int) []byte {
	t.Helper()
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random: %v", err)
	}
	return buf
}

func randomHex(t *testing.T, size int) string {
	t.Helper()
	return hex.EncodeToString(randomBytes(t, size))
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func buildDSN(host string, port uint16, database, user, password string) string {
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, strconv.Itoa(int(port))),
		Path:     "/" + database,
		RawQuery: url.Values{"sslmode": {"disable"}}.Encode(),
	}).String()
}

func newStorageHarness(t *testing.T) *storageHarness {
	t.Helper()
	raw := os.Getenv(adminDSNEnv)
	if raw == "" {
		t.Skipf("set %s (loopback admin DSN) to run LC1-07 real-PostgreSQL integration tests", adminDSNEnv)
	}
	adminCfg, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatalf("invalid %s: %v", adminDSNEnv, err)
	}
	if !loopback(adminCfg.Host) {
		t.Fatalf("%s must target a loopback host", adminDSNEnv)
	}

	ctx := context.Background()
	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}

	suffix := randomHex(t, 5)
	harness := &storageHarness{
		ownerRole:   "lc1_07_verify_owner_" + suffix,
		runtimeRole: "lc1_07_verify_runtime_" + suffix,
		database:    "lc1_07_verify_" + suffix,
		admin:       admin,
	}
	ownerPassword := randomHex(t, 16)
	runtimePassword := randomHex(t, 16)

	// Registered before any DDL so a partial setup failure still cleans up the
	// objects that were created. connect() cleanups run first (LIFO).
	t.Cleanup(func() {
		dropCtx := context.Background()
		if _, err := admin.Exec(dropCtx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, harness.database)); err != nil {
			t.Errorf("drop database: %v", err)
		}
		for _, role := range []string{harness.runtimeRole, harness.ownerRole} {
			if _, err := admin.Exec(dropCtx, fmt.Sprintf(`DROP ROLE IF EXISTS %q`, role)); err != nil {
				t.Errorf("drop role %s: %v", role, err)
			}
		}
		admin.Close(dropCtx)
	})

	execAdmin := func(statement string) {
		t.Helper()
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("admin statement failed: %v", err)
		}
	}
	execAdmin(fmt.Sprintf(`CREATE ROLE %q LOGIN PASSWORD '%s'`, harness.ownerRole, ownerPassword))
	execAdmin(fmt.Sprintf(`CREATE ROLE %q LOGIN PASSWORD '%s'`, harness.runtimeRole, runtimePassword))
	execAdmin(fmt.Sprintf(`CREATE DATABASE %q OWNER %q`, harness.database, harness.ownerRole))

	harness.ownerDSN = buildDSN(adminCfg.Host, adminCfg.Port, harness.database, harness.ownerRole, ownerPassword)
	harness.runtimeDSN = buildDSN(adminCfg.Host, adminCfg.Port, harness.database, harness.runtimeRole, runtimePassword)
	return harness
}

func (h *storageHarness) connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func (h *storageHarness) owner(t *testing.T) *pgx.Conn   { return h.connect(t, h.ownerDSN) }
func (h *storageHarness) runtime(t *testing.T) *pgx.Conn { return h.connect(t, h.runtimeDSN) }

// migrate applies the embedded migrations as the owner using the same goose
// provider the migrate command uses.
func (h *storageHarness) migrate(t *testing.T) {
	t.Helper()
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
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
}

// grantRuntime applies the runtime privilege boundary the deployment docs
// describe: no schema CREATE, mutable access only on the clinical tables,
// authentication reads/inserts only where the runtime needs them, and no owner
// membership.
func (h *storageHarness) grantRuntime(t *testing.T) {
	t.Helper()
	owner := h.owner(t)
	ctx := context.Background()
	role := h.runtimeRole
	statements := []string{
		`REVOKE CREATE ON SCHEMA public FROM PUBLIC`,
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %q`, role),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON cases, reads, marks, ai_jobs, candidate_decisions TO %q`, role),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE ON users TO %q`, role),
		fmt.Sprintf(`GRANT SELECT, INSERT, DELETE ON auth_sessions TO %q`, role),
		fmt.Sprintf(`GRANT SELECT, INSERT ON events TO %q`, role),
		fmt.Sprintf(`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %q`, role),
		fmt.Sprintf(`GRANT EXECUTE ON FUNCTION lc1_finite_triple(double precision[]) TO %q`, role),
	}
	for _, statement := range statements {
		if _, err := owner.Exec(ctx, statement); err != nil {
			t.Fatalf("grant statement failed: %v", err)
		}
	}
}

func assertSQLState(t *testing.T, err error, code, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: statement unexpectedly succeeded, want SQLSTATE %s", label, code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: error = %v, want *pgconn.PgError", label, err)
	}
	if pgErr.Code != code {
		t.Fatalf("%s: SQLSTATE = %s (%s), want %s", label, pgErr.Code, pgErr.Message, code)
	}
}

// seedUser inserts one active user with a synthetic (non-real) lookup value and
// returns its id. Tests never use credential material.
func seedUser(t *testing.T, tx pgx.Tx, name string) int64 {
	t.Helper()
	var id int64
	err := tx.QueryRow(context.Background(),
		`INSERT INTO users (display_name, pin_lookup, pin_hash)
		 VALUES ($1, decode($2, 'hex'), 'synthetic') RETURNING id`, name, randomHex(t, 32)).Scan(&id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedCaseRead inserts one case and one initial read and returns their ids.
func seedCaseRead(t *testing.T, tx pgx.Tx, seriesUID string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var caseID int64
	err := tx.QueryRow(ctx,
		`INSERT INTO cases (source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid)
		 VALUES ('LIDC-IDRI/QA-SYNTHETIC', '1.2.3.4.5', $1, 'orthanc-1', '1.2.3.4.7') RETURNING id`, seriesUID).Scan(&caseID)
	if err != nil {
		t.Fatalf("seed case: %v", err)
	}
	readerID := seedUser(t, tx, "dr-synthetic")
	var readID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO reads (case_id, kind, reader_user_id) VALUES ($1, 'initial', $2) RETURNING id`, caseID, readerID).Scan(&readID)
	if err != nil {
		t.Fatalf("seed read: %v", err)
	}
	return caseID, readID
}

const (
	validTripleA = `'{-102.3, 45.1, -180.5}'::float8[]`
	validTripleB = `'{-90.2, 33.0, -180.5}'::float8[]`
	validCenter  = `'{-96.25, 39.05, -180.5}'::float8[]`
)

// markInsert builds a parameterized mark insert (read id is $1) that callers
// may extend with RETURNING.
func markInsert(readID int64, p1, p2, center, diameter, origin, candidateID, normal string) (string, []any) {
	return `INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin, ai_candidate_id, view_plane_normal)
	        VALUES ($1, ` + p1 + `, ` + p2 + `, ` + center + `, ` + diameter + `, ` + origin + `, ` + candidateID + `, ` + normal + `)`,
		[]any{readID}
}

func TestLC1StorageConstraints(t *testing.T) {
	h := newStorageHarness(t)
	h.migrate(t)
	owner := h.owner(t)
	ctx := context.Background()

	t.Run("valid_round_trip", func(t *testing.T) {
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)

		caseID, readID := seedCaseRead(t, tx, "1.2.3.4.6")
		statement, args := markInsert(readID, validTripleA, validTripleB, validCenter, "17.0", "'reader'", "NULL", `'{0, 0, 1}'::float8[]`)
		var markID int64
		if err := tx.QueryRow(ctx, statement+" RETURNING id", args...).Scan(&markID); err != nil {
			t.Fatalf("insert valid mark: %v", err)
		}

		var p1 []float64
		var diameter float64
		if err := tx.QueryRow(ctx, `SELECT p1_mm, diameter_mm FROM marks WHERE id = $1`, markID).Scan(&p1, &diameter); err != nil {
			t.Fatalf("read back mark: %v", err)
		}
		if len(p1) != 3 || p1[0] != -102.3 || p1[2] != -180.5 || diameter != 17.0 {
			t.Fatalf("round trip = p1 %v diameter %v", p1, diameter)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO candidate_decisions (read_id, candidate_id, decision, dismiss_reason)
			 VALUES ($1, 'c1', 'dismissed', 'vessel, not a nodule')`, readID); err != nil {
			t.Fatalf("insert dismissed decision: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO candidate_decisions (read_id, candidate_id, decision, dismiss_reason)
			 VALUES ($1, 'c2', 'accepted', NULL)`, readID); err != nil {
			t.Fatalf("insert accepted decision: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ai_jobs (case_id, status) VALUES ($1, 'queued')`, caseID); err != nil {
			t.Fatalf("insert job: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO events (case_id, actor, action) VALUES ($1, 'dr-synthetic', 'case_viewed')`, caseID); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	})

	t.Run("geometry_rejections", func(t *testing.T) {
		tx, err := owner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		_, readID := seedCaseRead(t, tx, "1.2.3.4.8")

		badTriples := []struct {
			label string
			expr  string
			state string
		}{
			{"wrong_length_2", `'{1, 2}'::float8[]`, "23514"},
			{"wrong_length_4", `'{1, 2, 3, 4}'::float8[]`, "23514"},
			{"empty", `'{}'::float8[]`, "23514"},
			{"multidimensional", `'{{1,2,3}}'::float8[]`, "23514"},
			{"lower_bound_2", `'[2:4]={1,2,3}'::float8[]`, "23514"},
			{"null_element", `'{1, NULL, 3}'::float8[]`, "23514"},
			{"nan_element", `'{1, NaN, 3}'::float8[]`, "23514"},
			{"plus_inf", `'{1, Infinity, 3}'::float8[]`, "23514"},
			{"minus_inf", `'{1, -Infinity, 3}'::float8[]`, "23514"},
		}
		for _, tc := range badTriples {
			t.Run("p1_"+tc.label, func(t *testing.T) {
				sp, err := tx.Begin(ctx)
				if err != nil {
					t.Fatalf("savepoint: %v", err)
				}
				statement, args := markInsert(readID, tc.expr, validTripleB, validCenter, "17.0", "'reader'", "NULL", "NULL")
				_, err = sp.Exec(ctx, statement, args...)
				assertSQLState(t, err, tc.state, tc.label)
				if err := sp.Rollback(ctx); err != nil {
					t.Fatalf("rollback savepoint: %v", err)
				}
				var count int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM marks WHERE read_id = $1`, readID).Scan(&count); err != nil {
					t.Fatalf("count marks: %v", err)
				}
				if count != 0 {
					t.Fatalf("rejected mark left %d rows", count)
				}
			})
		}

		t.Run("null_required_array_is_not_null", func(t *testing.T) {
			sp, _ := tx.Begin(ctx)
			defer sp.Rollback(ctx)
			_, err := sp.Exec(ctx,
				`INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin)
				 VALUES ($1, NULL, `+validTripleB+`, `+validCenter+`, 17.0, 'reader')`, readID)
			assertSQLState(t, err, "23502", "null required array")
		})

		t.Run("bad_diameter", func(t *testing.T) {
			for _, diameter := range []string{"'0'", "'-1'", "'NaN'", "'Infinity'", "'-Infinity'"} {
				t.Run(diameter, func(t *testing.T) {
					sp, _ := tx.Begin(ctx)
					defer sp.Rollback(ctx)
					statement, args := markInsert(readID, validTripleA, validTripleB, validCenter, diameter, "'reader'", "NULL", "NULL")
					_, err := sp.Exec(ctx, statement, args...)
					assertSQLState(t, err, "23514", "diameter "+diameter)
				})
			}
		})

		t.Run("coincident_endpoints", func(t *testing.T) {
			sp, _ := tx.Begin(ctx)
			defer sp.Rollback(ctx)
			statement, args := markInsert(readID, validTripleA, validTripleA, validCenter, "17.0", "'reader'", "NULL", "NULL")
			_, err := sp.Exec(ctx, statement, args...)
			assertSQLState(t, err, "23514", "coincident endpoints")
		})

		t.Run("invalid_normal", func(t *testing.T) {
			for _, normal := range []string{`'{1, 2}'::float8[]`, `'{1, NaN, 3}'::float8[]`, `'{}'::float8[]`} {
				sp, _ := tx.Begin(ctx)
				statement, args := markInsert(readID, validTripleA, validTripleB, validCenter, "17.0", "'reader'", "NULL", normal)
				_, err := sp.Exec(ctx, statement, args...)
				assertSQLState(t, err, "23514", "normal "+normal)
				sp.Rollback(ctx)
			}
		})
	})

	t.Run("mark_origin_and_candidate_coupling", func(t *testing.T) {
		tx, _ := owner.Begin(ctx)
		defer tx.Rollback(ctx)
		_, readID := seedCaseRead(t, tx, "1.2.3.4.9")

		cases := []struct {
			label     string
			origin    string
			candidate string
			wantError bool
		}{
			{"from_ai_requires_id", "'from_ai_candidate'", "NULL", true},
			{"from_ai_requires_nonblank", "'from_ai_candidate'", "''", true},
			{"from_ai_requires_nonblank_tab", "'from_ai_candidate'", "E'\\t'", true},
			{"from_ai_valid", "'from_ai_candidate'", "'c1'", false},
			{"reader_must_not_have_id", "'reader'", "'c1'", true},
			{"reader_after_ai_must_not_have_id", "'reader_after_ai'", "'c1'", true},
			{"reader_valid", "'reader'", "NULL", false},
			{"invalid_origin_value", "'bogus'", "NULL", true},
		}
		for _, tc := range cases {
			t.Run(tc.label, func(t *testing.T) {
				sp, _ := tx.Begin(ctx)
				defer sp.Rollback(ctx)
				statement, args := markInsert(readID, validTripleA, validTripleB, validCenter, "17.0", tc.origin, tc.candidate, "NULL")
				_, err := sp.Exec(ctx, statement, args...)
				if tc.wantError {
					assertSQLState(t, err, "23514", tc.label)
				} else if err != nil {
					t.Fatalf("%s: unexpected error %v", tc.label, err)
				}
			})
		}
	})

	t.Run("state_and_kind_checks", func(t *testing.T) {
		tx, _ := owner.Begin(ctx)
		defer tx.Rollback(ctx)
		caseID, readID := seedCaseRead(t, tx, "1.2.3.4.10")

		sp, _ := tx.Begin(ctx)
		_, err := sp.Exec(ctx, `UPDATE cases SET status = 'AI_REVEALED' WHERE id = $1`, caseID)
		assertSQLState(t, err, "23514", "invalid case status")
		sp.Rollback(ctx)

		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx, `INSERT INTO reads (case_id, kind, reader_user_id)
			SELECT $1, 'draft', reader_user_id FROM reads WHERE id = $2`, caseID, readID)
		assertSQLState(t, err, "23514", "invalid read kind")
		sp.Rollback(ctx)

		var jobID int64
		if err := tx.QueryRow(ctx, `INSERT INTO ai_jobs (case_id, status) VALUES ($1, 'queued') RETURNING id`, caseID).Scan(&jobID); err != nil {
			t.Fatalf("insert job: %v", err)
		}
		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx, `UPDATE ai_jobs SET status = 'unknown' WHERE id = $1`, jobID)
		assertSQLState(t, err, "23514", "invalid job status")
		sp.Rollback(ctx)

		if _, err := tx.Exec(ctx, `INSERT INTO candidate_decisions (read_id, candidate_id, decision) VALUES ($1, 'c9', 'accepted')`, readID); err != nil {
			t.Fatalf("insert decision: %v", err)
		}
		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx, `UPDATE candidate_decisions SET decision = 'maybe' WHERE read_id = $1 AND candidate_id = 'c9'`, readID)
		assertSQLState(t, err, "23514", "invalid decision")
		sp.Rollback(ctx)

		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx, `INSERT INTO candidate_decisions (read_id, candidate_id, decision) VALUES ($1, '   ', 'accepted')`, readID)
		assertSQLState(t, err, "23514", "blank candidate id")
		sp.Rollback(ctx)

		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx, `INSERT INTO candidate_decisions (read_id, candidate_id, decision) VALUES ($1, E'\t', 'accepted')`, readID)
		assertSQLState(t, err, "23514", "tab-only candidate id")
		sp.Rollback(ctx)
	})

	t.Run("dismiss_reason_rules", func(t *testing.T) {
		tx, _ := owner.Begin(ctx)
		defer tx.Rollback(ctx)
		_, readID := seedCaseRead(t, tx, "1.2.3.4.11")

		rejected := []struct {
			label    string
			decision string
			reason   string
		}{
			{"dismissed_null", "'dismissed'", "NULL"},
			{"dismissed_empty", "'dismissed'", "''"},
			{"dismissed_spaces", "'dismissed'", "'   '"},
			{"dismissed_tab", "'dismissed'", "E'\\t'"},
			{"dismissed_newline", "'dismissed'", "E'\\n'"},
			{"dismissed_cr", "'dismissed'", "E'\\r'"},
			{"accepted_with_reason", "'accepted'", "'why'"},
		}
		for _, tc := range rejected {
			t.Run(tc.label, func(t *testing.T) {
				sp, _ := tx.Begin(ctx)
				defer sp.Rollback(ctx)
				_, err := sp.Exec(ctx,
					`INSERT INTO candidate_decisions (read_id, candidate_id, decision, dismiss_reason)
					 VALUES ($1, 'c1', `+tc.decision+`, `+tc.reason+`)`, readID)
				assertSQLState(t, err, "23514", tc.label)
			})
		}

		t.Run("trimmed_reason_is_valid", func(t *testing.T) {
			sp, _ := tx.Begin(ctx)
			defer sp.Rollback(ctx)
			_, err := sp.Exec(ctx,
				`INSERT INTO candidate_decisions (read_id, candidate_id, decision, dismiss_reason)
				 VALUES ($1, 'c1', 'dismissed', E'  vessel  ')`, readID)
			if err != nil {
				t.Fatalf("trimmed reason rejected: %v", err)
			}
		})
	})

	t.Run("uniqueness_and_foreign_keys", func(t *testing.T) {
		tx, _ := owner.Begin(ctx)
		defer tx.Rollback(ctx)
		_, readID := seedCaseRead(t, tx, "1.2.3.4.12")

		sp, _ := tx.Begin(ctx)
		_, err := sp.Exec(ctx,
			`INSERT INTO cases (source, study_instance_uid, series_instance_uid, orthanc_series_id, frame_of_reference_uid)
			 VALUES ('s', '1.1', '1.2.3.4.12', 'o', '1.1')`)
		assertSQLState(t, err, "23505", "duplicate series uid")
		sp.Rollback(ctx)

		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx,
			`INSERT INTO reads (case_id, kind, reader_user_id)
			 SELECT case_id, 'initial', reader_user_id FROM reads WHERE id = $1`, readID)
		assertSQLState(t, err, "23505", "duplicate case/kind")
		sp.Rollback(ctx)

		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx, `INSERT INTO reads (case_id, kind, reader_user_id)
			SELECT 999999, 'initial', reader_user_id FROM reads WHERE id = $1`, readID)
		assertSQLState(t, err, "23503", "orphan read case")
		sp.Rollback(ctx)

		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx, `INSERT INTO marks (read_id, p1_mm, p2_mm, center_mm, diameter_mm, origin) VALUES (999999, `+validTripleA+`, `+validTripleB+`, `+validCenter+`, 1.0, 'reader')`)
		assertSQLState(t, err, "23503", "orphan mark read")
		sp.Rollback(ctx)

		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx, `INSERT INTO events (case_id, actor, action) VALUES (999999, 'a', 'b')`)
		assertSQLState(t, err, "23503", "orphan event case")
		sp.Rollback(ctx)

		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx, `INSERT INTO candidate_decisions (read_id, candidate_id, decision) VALUES (999999, 'c1', 'accepted')`)
		assertSQLState(t, err, "23503", "orphan decision read")
		sp.Rollback(ctx)

		if _, err := tx.Exec(ctx, `INSERT INTO candidate_decisions (read_id, candidate_id, decision) VALUES ($1, 'c1', 'accepted')`, readID); err != nil {
			t.Fatalf("insert decision: %v", err)
		}
		sp, _ = tx.Begin(ctx)
		_, err = sp.Exec(ctx, `INSERT INTO candidate_decisions (read_id, candidate_id, decision) VALUES ($1, 'c1', 'accepted')`, readID)
		assertSQLState(t, err, "23505", "duplicate decision key")
		sp.Rollback(ctx)
	})
}

// TestLC1StorageMigrationRollback proves a failing migration leaves no half
// schema: the same goose provider is run against a test-owned filesystem whose
// migration creates a table and then fails.
func TestLC1StorageMigrationRollback(t *testing.T) {
	h := newStorageHarness(t)
	broken := fstest.MapFS{
		"00001_lc1_storage.sql": &fstest.MapFile{Data: []byte(
			"-- +goose Up\n" +
				"CREATE TABLE cases (id bigint primary key);\n" +
				"CREATE TABLE marks (id bigint primary key);\n" +
				"SELECT 1 / 0;\n" +
				"-- +goose Down\n" +
				"DROP TABLE marks;\n" +
				"DROP TABLE cases;\n")},
	}
	db, err := sql.Open("pgx", h.ownerDSN)
	if err != nil {
		t.Fatalf("open owner handle: %v", err)
	}
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, broken)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}
	defer provider.Close()

	ctx := context.Background()
	if _, err := provider.Up(ctx); err == nil {
		t.Fatalf("intentionally broken migration unexpectedly succeeded")
	}

	owner := h.owner(t)
	var tables int
	business := []string{"cases", "reads", "marks", "ai_jobs", "candidate_decisions", "events"}
	if err := owner.QueryRow(ctx,
		`SELECT count(*) FROM pg_class WHERE relkind = 'r' AND relname = ANY($1)`, business).Scan(&tables); err != nil {
		t.Fatalf("count business tables: %v", err)
	}
	if tables != 0 {
		t.Fatalf("failed migration left %d business tables", tables)
	}
	var helperMissing bool
	if err := owner.QueryRow(ctx, `SELECT to_regprocedure('lc1_finite_triple(double precision[])') IS NULL`).Scan(&helperMissing); err != nil {
		t.Fatalf("to_regprocedure: %v", err)
	}
	if !helperMissing {
		t.Fatalf("failed migration left the helper function behind")
	}
}

// backendPID returns the server process id of one connection so a test can
// observe which sessions it blocks.
func backendPID(t *testing.T, conn *pgx.Conn) int32 {
	t.Helper()
	var pid int32
	if err := conn.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	return pid
}

// waitForBlockedSessions observes real lock contention on this harness's
// database: it returns once at least count sessions are blocked behind
// blockerPID, directly or transitively, and fails after five seconds. A
// transitive closure is required because PostgreSQL queues a second waiter
// behind the first waiter rather than behind the original holder. It replaces a
// timing guess with an observed, bounded wait.
func waitForBlockedSessions(t *testing.T, h *storageHarness, blockerPID int32, count int) {
	t.Helper()
	const query = `
		WITH RECURSIVE blocked(pid) AS (
			SELECT pid FROM pg_stat_activity
			 WHERE datname = $1 AND $2 = ANY (pg_blocking_pids(pid))
			UNION
			SELECT activity.pid FROM pg_stat_activity activity
			 JOIN blocked ON blocked.pid = ANY (pg_blocking_pids(activity.pid))
			 WHERE activity.datname = $1
		)
		SELECT count(*) FROM blocked`
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked int
		if err := h.admin.QueryRow(context.Background(), query, h.database, blockerPID).Scan(&blocked); err != nil {
			t.Fatalf("observe blocked sessions: %v", err)
		}
		if blocked >= count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("observed %d sessions blocked behind pid %d, want at least %d", blocked, blockerPID, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
