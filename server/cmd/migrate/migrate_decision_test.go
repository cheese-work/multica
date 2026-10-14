package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDecisionMigrationsUpDownUp(test *testing.T) {
	base := openTestPool(test)
	ctx, cancel := context.WithTimeout(test.Context(), time.Minute)
	defer cancel()
	schema := createScratchSchema(test, ctx, base, "decision_migrator_")
	pool := openTestPoolWithSearchPath(test, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE comment (id uuid PRIMARY KEY, content text NOT NULL); INSERT INTO comment VALUES (gen_random_uuid(), 'legacy')`); err != nil {
		test.Fatal(err)
	}
	for step, direction := range []string{"up", "down", "up"} {
		versions := []string{
			"597_decision_parents", "598_decision_request_id_index", "599_decision_intent_id_index",
			"600_decision_dependents", "601_decision_answer_id_index", "602_decision_target_identity_index",
			"603_decision_live_answer_index", "604_decision_open_intent_index", "605_decision_answer_comment_index",
			"606_decision_request_comment_index", "607_decision_answer_messages_index", "608_decision_request_scope_index",
			"609_decision_worker_lease_name_index", "610_decision_class_change_request_index", "611_decision_answer_request_index",
			"612_decision_target_primary_key", "613_decision_comment_kind_validate",
			"615_decision_request_comment_v2_index", "616_decision_request_comment_index_retire",
			"617_decision_comment_request_check", "618_decision_comment_request_validate",
		}
		if direction == "down" {
			slices.Reverse(versions)
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
		if step == 0 {
			for _, target := range []string{"612_decision_target_primary_key", "611_decision_answer_request_index"} {
				test.Run("bounded_to_"+target, func(test *testing.T) {
					downVersions := append([]string(nil), versions...)
					slices.Reverse(downVersions)
					files, err := boundDownFiles(realMigrationFiles(test, downVersions, "down"), target)
					if err != nil {
						test.Fatal(err)
					}
					if err := runMigrations(ctx, pool, runOptions{
						Direction: "down", Files: files, SchemaMigrationsTable: schema + ".schema_migrations",
						AdvisoryLockKey: int64(rand.Uint64()&0x7fffffffffffffff) | 1,
						Hooks:           hooksForDirection("down"), Conditions: conditionsForDirection("down"),
					}); err != nil {
						test.Fatalf("bounded rollback to %s: %v", target, err)
					}
					if err := runMigrations(ctx, pool, runOptions{
						Direction: "up", Files: realMigrationFiles(test, versions, "up"), SchemaMigrationsTable: schema + ".schema_migrations",
						AdvisoryLockKey: int64(rand.Uint64()&0x7fffffffffffffff) | 1,
						Hooks:           hooksForDirection("up"), Conditions: conditionsForDirection("up"),
					}); err != nil {
						test.Fatalf("reapply after bounded rollback to %s: %v", target, err)
					}
					var primaryKeys int
					if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'decision_intent_target'::regclass AND contype = 'p'`).Scan(&primaryKeys); err != nil || primaryKeys != 1 {
						test.Fatalf("primary key not restored: count=%d err=%v", primaryKeys, err)
					}
				})
			}
		}
		var indexes, comments int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_index index_row JOIN pg_class index_name ON index_name.oid = index_row.indexrelid JOIN pg_namespace namespace ON namespace.oid = index_name.relnamespace WHERE namespace.nspname = $1 AND index_row.indisvalid AND (index_name.relname LIKE 'decision_%' OR index_name.relname LIKE 'comment_decision_%')`, schema).Scan(&indexes); err != nil {
			test.Fatal(err)
		}
		want := 13
		if direction == "down" {
			want = 0
		}
		if indexes != want {
			test.Fatalf("after %s: %d valid indexes, want %d", direction, indexes, want)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE content = 'legacy'`).Scan(&comments); err != nil || comments != 1 {
			test.Fatalf("after %s: baseline comments=%d, err=%v", direction, comments, err)
		}
	}
}

func TestDecisionCommentMigrationLockTimeout(test *testing.T) {
	base := openTestPool(test)
	ctx, cancel := context.WithTimeout(test.Context(), 10*time.Second)
	defer cancel()
	schema := createScratchSchema(test, ctx, base, "decision_comment_lock_")
	pool := openTestPoolWithSearchPath(test, schema)
	readerPool := openTestPoolWithSearchPath(test, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE comment (id uuid PRIMARY KEY, content text NOT NULL); INSERT INTO comment VALUES (gen_random_uuid(), 'legacy')`); err != nil {
		test.Fatal(err)
	}
	var originalTimeout string
	if err := pool.QueryRow(ctx, "SHOW lock_timeout").Scan(&originalTimeout); err != nil {
		test.Fatal(err)
	}
	reader, err := readerPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer reader.Rollback(test.Context())
	if _, err := reader.Exec(ctx, "SELECT * FROM comment"); err != nil {
		test.Fatal(err)
	}
	run := func(version string) error {
		return runMigrations(ctx, pool, runOptions{
			Direction:             "up",
			Files:                 realMigrationFiles(test, []string{version}, "up"),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		})
	}
	err = run("597_decision_parents")
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "55P03" {
		test.Fatalf("comment DDL must fail at its local lock timeout, not the context deadline: %v", err)
	}
	var recorded int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&recorded); err != nil || recorded != 0 {
		test.Fatalf("lock-timeout failure must remain unrecorded: count=%d err=%v", recorded, err)
	}
	var timeoutAfter string
	if err := pool.QueryRow(ctx, "SHOW lock_timeout").Scan(&timeoutAfter); err != nil || timeoutAfter != originalTimeout {
		test.Fatalf("local timeout leaked after rollback: before=%s after=%s err=%v", originalTimeout, timeoutAfter, err)
	}
	if err := reader.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if err := run("597_decision_parents"); err != nil {
		test.Fatalf("retry after releasing the reader: %v", err)
	}
	validationReader, err := readerPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer validationReader.Rollback(test.Context())
	if _, err := validationReader.Exec(ctx, "SELECT * FROM comment"); err != nil {
		test.Fatal(err)
	}
	if err := run("613_decision_comment_kind_validate"); err != nil {
		test.Fatalf("separate CHECK validation must coexist with an ordinary reader: %v", err)
	}
	err = run("617_decision_comment_request_check")
	if !errors.As(err, &pgError) || pgError.Code != "55P03" {
		test.Fatalf("comment request CHECK must fail at its local lock timeout: %v", err)
	}
	if err := validationReader.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if err := run("617_decision_comment_request_check"); err != nil {
		test.Fatalf("request CHECK retry after releasing the reader: %v", err)
	}
	requestReader, err := readerPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer requestReader.Rollback(test.Context())
	if _, err := requestReader.Exec(ctx, "SELECT * FROM comment"); err != nil {
		test.Fatal(err)
	}
	if err := run("618_decision_comment_request_validate"); err != nil {
		test.Fatalf("separate request CHECK validation must coexist with an ordinary reader: %v", err)
	}
}

func TestDecisionCommentRequestCheckReplaysAfterUnrecordedCommit(test *testing.T) {
	base := openTestPool(test)
	ctx, cancel := context.WithTimeout(test.Context(), 10*time.Second)
	defer cancel()
	schema := createScratchSchema(test, ctx, base, "decision_comment_replay_")
	pool := openTestPoolWithSearchPath(test, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE comment (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), content text NOT NULL)`); err != nil {
		test.Fatal(err)
	}
	run := func(versions ...string) error {
		return runMigrations(ctx, pool, runOptions{
			Direction:             "up",
			Files:                 realMigrationFiles(test, versions, "up"),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		})
	}
	if err := run("597_decision_parents", "613_decision_comment_kind_validate"); err != nil {
		test.Fatal(err)
	}
	checkSQL, err := os.ReadFile(realMigrationFiles(test, []string{"617_decision_comment_request_check"}, "up")[0])
	if err != nil {
		test.Fatal(err)
	}
	transaction, err := pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(test.Context())
	if _, err := transaction.Exec(ctx, string(checkSQL)); err != nil {
		test.Fatal(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	var recorded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '617_decision_comment_request_check'`).Scan(&recorded); err != nil || recorded != 0 {
		test.Fatalf("DDL-only commit must remain unrecorded: count=%d err=%v", recorded, err)
	}
	if err := run("617_decision_comment_request_check", "618_decision_comment_request_validate"); err != nil {
		test.Fatalf("replay after an unrecorded DDL commit: %v", err)
	}
	var constraints int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'comment'::regclass AND conname = 'comment_decision_request_check' AND convalidated`).Scan(&constraints); err != nil || constraints != 1 {
		test.Fatalf("replay must retain one validated CHECK: count=%d err=%v", constraints, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version IN ('617_decision_comment_request_check', '618_decision_comment_request_validate')`).Scan(&recorded); err != nil || recorded != 2 {
		test.Fatalf("replay must record both migrations: count=%d err=%v", recorded, err)
	}
}

func TestDecisionCommentRequestValidationRejectsInvalidRows(test *testing.T) {
	base := openTestPool(test)
	ctx, cancel := context.WithTimeout(test.Context(), 10*time.Second)
	defer cancel()
	schema := createScratchSchema(test, ctx, base, "decision_comment_validation_")
	pool := openTestPoolWithSearchPath(test, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE comment (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), content text NOT NULL)`); err != nil {
		test.Fatal(err)
	}
	run := func(versions ...string) error {
		return runMigrations(ctx, pool, runOptions{
			Direction:             "up",
			Files:                 realMigrationFiles(test, versions, "up"),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		})
	}
	if err := run("597_decision_parents", "613_decision_comment_kind_validate"); err != nil {
		test.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO comment (content, decision_comment_kind) VALUES ('invalid', 'ask')`); err != nil {
		test.Fatal(err)
	}
	for _, kind := range []string{"quote", "disclaim", "delivery", "veto"} {
		if _, err := pool.Exec(ctx, `INSERT INTO comment (content, decision_answer_id, decision_comment_kind) VALUES ('valid answer', gen_random_uuid(), $1)`, kind); err != nil {
			test.Fatal(err)
		}
	}
	if err := run("617_decision_comment_request_check"); err != nil {
		test.Fatal(err)
	}
	err := run("618_decision_comment_request_validate")
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "23514" || pgError.ConstraintName != "comment_decision_request_check" {
		test.Fatalf("validation must reject existing kind without a matching stamp: %v", err)
	}
	var recorded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '618_decision_comment_request_validate'`).Scan(&recorded); err != nil || recorded != 0 {
		test.Fatalf("failed validation must remain unrecorded: count=%d err=%v", recorded, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE comment SET decision_request_id = gen_random_uuid() WHERE content = 'invalid'`); err != nil {
		test.Fatal(err)
	}
	if err := run("618_decision_comment_request_validate"); err != nil {
		test.Fatalf("validation retry after repairing the fixture: %v", err)
	}
	var answerComments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE content = 'valid answer' AND decision_answer_id IS NOT NULL AND decision_request_id IS NULL`).Scan(&answerComments); err != nil || answerComments != 4 {
		test.Fatalf("validation must preserve answer-only comments: count=%d err=%v", answerComments, err)
	}
}
