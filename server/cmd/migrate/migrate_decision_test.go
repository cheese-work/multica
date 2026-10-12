package main

import (
	"context"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
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
	for _, direction := range []string{"up", "down", "up"} {
		versions := []string{
			"596_decision_parents", "597_decision_request_id_index", "598_decision_intent_id_index",
			"599_decision_dependents", "600_decision_answer_id_index", "601_decision_target_identity_index",
			"602_decision_live_answer_index", "603_decision_open_intent_index", "604_decision_answer_comment_index",
			"605_decision_request_comment_index", "606_decision_answer_messages_index", "607_decision_request_scope_index",
			"608_decision_worker_lease_name_index", "609_decision_class_change_request_index", "610_decision_answer_request_index",
			"611_decision_target_primary_key",
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
