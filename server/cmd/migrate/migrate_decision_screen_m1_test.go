package main

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M1 of the decision screen plan r4 (CHE-5480, CHE-5995): additive migration for
// default_apply, undo, the backing lane, picks, snoozes and the request route.

var decisionScreenM1Versions = []string{"999s_decision_screen_m1", "999t_decision_backing_lane_uidx"}

func runDecisionScreenM1(test *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, direction string) {
	test.Helper()
	versions := append([]string(nil), decisionScreenM1Versions...)
	if direction == "down" {
		versions[0], versions[1] = versions[1], versions[0]
	}
	if err := runMigrations(ctx, pool, runOptions{
		Direction:             direction,
		Files:                 realMigrationFiles(test, versions, direction),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection(direction),
		Conditions:            conditionsForDirection(direction),
	}); err != nil {
		test.Fatalf("migrate %s: %v", direction, err)
	}
}

// m1Fixture builds the two decision tables in a scratch schema with only the columns
// M1 touches, applies the real 871 and 873 migrations so the CHECK constraints are the
// current ones, then applies M1. The shared test database is not migrated to main, so
// it cannot supply the tables.
func m1Fixture(test *testing.T) (context.Context, *pgxpool.Pool, string) {
	test.Helper()
	base := openTestPool(test)
	ctx, cancel := context.WithTimeout(test.Context(), time.Minute)
	test.Cleanup(cancel)
	schema := createScratchSchema(test, ctx, base, "decision_m1_")
	pool := openTestPoolWithSearchPath(test, schema)
	mustExec(test, ctx, pool, `
		CREATE TABLE decision_request (
			id uuid NOT NULL DEFAULT gen_random_uuid(), workspace_id uuid NOT NULL, issue_id uuid NOT NULL,
			asking_agent_id uuid NOT NULL, human_user_id uuid NOT NULL, chat_session_id uuid NOT NULL,
			escalation_class text NOT NULL, origin text NOT NULL);
		CREATE TABLE decision_answer (
			id uuid NOT NULL DEFAULT gen_random_uuid(), decision_request_id uuid NOT NULL,
			answered_by_task_id uuid, answered_by_user_id uuid, source_chat_session_id uuid, source_comment_id uuid,
			chat_message_ids uuid[] NOT NULL DEFAULT '{}', content_snapshot text NOT NULL, effective_at timestamptz NOT NULL,
			answer_source text NOT NULL DEFAULT 'agent_record', disclaim_reason text, disclaimed_at timestamptz,
			CONSTRAINT decision_answer_disclaim_reason_check CHECK (true));`)
	for _, version := range []string{"871_decision_card_required", "873_decision_card_answer_dm_source"} {
		mustExec(test, ctx, pool, string(mustRead(test, version+".up.sql")))
	}
	runDecisionScreenM1(test, ctx, pool, schema, "up")
	return ctx, pool, schema
}

func mustExec(test *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	test.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		test.Fatalf("%s: %v", sql, err)
	}
}

func mustFail(test *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	test.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err == nil {
		test.Fatalf("expected a constraint error: %s", sql)
	}
}

func m1Answer(test *testing.T, ctx context.Context, pool *pgxpool.Pool, source, reason string, withUser bool) error {
	test.Helper()
	requestID := uuid.New()
	mustExec(test, ctx, pool, `INSERT INTO decision_request (id, workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, escalation_class, origin)
		VALUES ($1, $2, $2, $2, $2, $2, 'reversible', 'asked')`, requestID, uuid.New())
	var user any
	if withUser {
		user = uuid.New()
	}
	_, err := pool.Exec(ctx, `INSERT INTO decision_answer (decision_request_id, answered_by_task_id, source_chat_session_id, chat_message_ids, content_snapshot, effective_at, answer_source, answered_by_user_id, disclaim_reason, disclaimed_at)
		VALUES ($1, NULL, NULL, '{}', 'x', now(), $2, $3, NULLIF($4, ''), CASE WHEN $4 = '' THEN NULL ELSE now() END)`,
		requestID, source, user, reason)
	return err
}

func TestDecisionScreenM1AnswerConstraints(test *testing.T) {
	ctx, pool, _ := m1Fixture(test)

	test.Run("default_apply is a system answer with no human, task or source", func(test *testing.T) {
		if err := m1Answer(test, ctx, pool, "default_apply", "", false); err != nil {
			test.Fatalf("default_apply answer rejected: %v", err)
		}
		if err := m1Answer(test, ctx, pool, "default_apply", "", true); err == nil {
			test.Fatal("default_apply with a human user accepted")
		}
	})
	test.Run("card answers stay valid and unknown sources stay rejected", func(test *testing.T) {
		if err := m1Answer(test, ctx, pool, "card", "", true); err != nil {
			test.Fatalf("card answer rejected: %v", err)
		}
		if err := m1Answer(test, ctx, pool, "robot", "", false); err == nil {
			test.Fatal("unknown answer_source accepted")
		}
	})
	test.Run("undo is a disclaim reason; old reasons stay; unknown stays rejected", func(test *testing.T) {
		for _, reason := range []string{"undo", "human", "card_required", "class_needs_owner"} {
			if err := m1Answer(test, ctx, pool, "card", reason, true); err != nil {
				test.Fatalf("disclaim_reason %q rejected: %v", reason, err)
			}
		}
		if err := m1Answer(test, ctx, pool, "card", "bogus", true); err == nil {
			test.Fatal("unknown disclaim_reason accepted")
		}
	})
	test.Run("undo_until is a nullable timestamptz", func(test *testing.T) {
		var dataType, nullable string
		if err := pool.QueryRow(ctx, `SELECT data_type, is_nullable FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'decision_answer' AND column_name = 'undo_until'`).Scan(&dataType, &nullable); err != nil {
			test.Fatalf("undo_until column missing: %v", err)
		}
		if dataType != "timestamp with time zone" || nullable != "YES" {
			test.Fatalf("undo_until = %s nullable=%s", dataType, nullable)
		}
	})
}

func TestDecisionScreenM1RequestColumns(test *testing.T) {
	ctx, pool, _ := m1Fixture(test)
	for column, want := range map[string]string{"route": "text", "would_auto_apply": "boolean", "auto_apply_rule_id": "uuid"} {
		var dataType, nullable string
		if err := pool.QueryRow(ctx, `SELECT data_type, is_nullable FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'decision_request' AND column_name = $1`, column).Scan(&dataType, &nullable); err != nil {
			test.Fatalf("decision_request.%s missing: %v", column, err)
		}
		if dataType != want || nullable != "YES" {
			test.Fatalf("decision_request.%s = %s nullable=%s, want %s nullable", column, dataType, nullable, want)
		}
	}
	insert := `INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, escalation_class, origin, route)
		VALUES ($1, $1, $1, $1, $1, 'reversible', 'asked', NULLIF($2, ''))`
	for _, route := range []string{"", "auto_apply", "human"} {
		mustExec(test, ctx, pool, insert, uuid.New(), route)
	}
	mustFail(test, ctx, pool, insert, uuid.New(), "manual")
}

func TestDecisionScreenM1BackingLane(test *testing.T) {
	ctx, pool, _ := m1Fixture(test)
	insert := `INSERT INTO decision_backing_lane (session_id, workspace_id, creator_id, topic_id) VALUES ($1, $2, $3, $4)`
	workspace, member, topic := uuid.New(), uuid.New(), uuid.New()
	mustExec(test, ctx, pool, insert, uuid.New(), workspace, member, topic)
	mustFail(test, ctx, pool, insert, uuid.New(), workspace, member, topic) // one lane per (workspace, member, topic)
	mustExec(test, ctx, pool, insert, uuid.New(), workspace, member, uuid.New())
	mustExec(test, ctx, pool, insert, uuid.New(), workspace, uuid.New(), topic)
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT i.indisvalid AND i.indisunique FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'decision_backing_lane_member_topic_uidx'`).Scan(&valid); err != nil || !valid {
		test.Fatalf("unique index decision_backing_lane_member_topic_uidx not valid: %v", err)
	}
}

func TestDecisionScreenM1PickAndSnooze(test *testing.T) {
	ctx, pool, _ := m1Fixture(test)

	test.Run("pick row is created at first sight and completed at the pick", func(test *testing.T) {
		request, member := uuid.New(), uuid.New()
		mustExec(test, ctx, pool, `INSERT INTO decision_pick (decision_request_id, revision, member_id, first_shown_at, surface) VALUES ($1, 1, $2, now(), 'screen')`, request, member)
		mustExec(test, ctx, pool, `UPDATE decision_pick SET picked_at = now(), read_ms = 1800, matched = false WHERE decision_request_id = $1 AND revision = 1`, request)
		mustFail(test, ctx, pool, `INSERT INTO decision_pick (decision_request_id, revision, member_id, surface) VALUES ($1, 1, $2, 'screen')`, request, member) // first_shown_at is required
	})
	test.Run("snooze ends within 24 h and has a paired condition", func(test *testing.T) {
		insert := `INSERT INTO decision_snooze (member_id, decision_request_id, created_at, until_at, cond_issue_id, cond_status)
			VALUES ($1, $2, '2026-11-15 00:00:00+00', $3::timestamptz, NULLIF($4, '')::uuid, NULLIF($5, ''))`
		mustExec(test, ctx, pool, insert, uuid.New(), uuid.New(), "2026-11-16 00:00:00+00", "", "")
		mustExec(test, ctx, pool, insert, uuid.New(), uuid.New(), "2026-11-15 01:00:00+00", uuid.NewString(), "done")
		mustFail(test, ctx, pool, insert, uuid.New(), uuid.New(), "2026-11-16 00:00:01+00", "", "")
		mustFail(test, ctx, pool, insert, uuid.New(), uuid.New(), "2026-11-15 01:00:00+00", uuid.NewString(), "")
		mustFail(test, ctx, pool, insert, uuid.New(), uuid.New(), "2026-11-15 01:00:00+00", "", "done")
		mustExec(test, ctx, pool, `UPDATE decision_snooze SET cleared_at = now(), clear_reason = 'condition_met'`)
	})
}

func TestDecisionScreenM1AdditiveOnly(test *testing.T) {
	ctx, pool, schema := m1Fixture(test)
	var foreignKeys int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = $1 AND c.contype = 'f' AND t.relname IN ('decision_backing_lane', 'decision_pick', 'decision_snooze')`, schema).Scan(&foreignKeys); err != nil {
		test.Fatal(err)
	}
	if foreignKeys != 0 {
		test.Fatalf("new tables carry %d foreign keys, want 0", foreignKeys)
	}
	for _, version := range decisionScreenM1Versions {
		up := string(mustRead(test, version+".up.sql"))
		down := string(mustRead(test, version+".down.sql"))
		for _, file := range []string{up, down} {
			if strings.Contains(strings.ToUpper(file), "CONCURRENTLY") && strings.Count(file, ";") != 1 {
				test.Fatalf("%s: a CONCURRENTLY file must hold exactly one statement", version)
			}
		}
		for _, destructive := range []string{"DROP TABLE", "DROP COLUMN", "TRUNCATE", "DELETE FROM"} {
			if strings.Contains(strings.ToUpper(up), destructive) {
				test.Fatalf("%s.up.sql is not additive: %s", version, destructive)
			}
		}
	}
}

func TestDecisionScreenM1UpDownUp(test *testing.T) {
	ctx, pool, schema := m1Fixture(test)
	// A replay after an uncertain acknowledgement must be harmless.
	runDecisionScreenM1(test, ctx, pool, schema, "up")
	if err := m1Answer(test, ctx, pool, "default_apply", "", false); err != nil {
		test.Fatalf("after replay: %v", err)
	}

	runDecisionScreenM1(test, ctx, pool, schema, "down")
	for _, table := range []string{"decision_backing_lane", "decision_pick", "decision_snooze"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+"."+table).Scan(&exists); err != nil || exists {
			test.Fatalf("after down: table %s still exists (err=%v)", table, err)
		}
	}
	var columns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = $1 AND
		((table_name = 'decision_answer' AND column_name = 'undo_until')
		 OR (table_name = 'decision_request' AND column_name IN ('route', 'would_auto_apply', 'auto_apply_rule_id')))`, schema).Scan(&columns); err != nil || columns != 0 {
		test.Fatalf("after down: %d M1 columns remain (err=%v)", columns, err)
	}
	// Down restores the previous constraints: the new values are refused again.
	if err := m1Answer(test, ctx, pool, "default_apply", "", false); err == nil {
		test.Fatal("after down: default_apply accepted")
	}
	if err := m1Answer(test, ctx, pool, "card", "undo", true); err == nil {
		test.Fatal("after down: undo accepted")
	}
	if err := m1Answer(test, ctx, pool, "card", "card_required", true); err != nil {
		test.Fatalf("after down: previous reason rejected: %v", err)
	}

	runDecisionScreenM1(test, ctx, pool, schema, "up")
	if err := m1Answer(test, ctx, pool, "card", "undo", true); err != nil {
		test.Fatalf("after re-up: %v", err)
	}
}

func mustRead(test *testing.T, name string) []byte {
	test.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
	if err != nil {
		test.Fatal(err)
	}
	return data
}
