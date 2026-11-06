package main

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"testing"
	"time"
)

// TestIssueDecisionParkMigrationMovesTheParkOutOfMetadata: up copies every
// object decision_park into its own table and removes the key; down writes the
// same record back. (CHE-3946)
func TestIssueDecisionParkMigrationMovesTheParkOutOfMetadata(test *testing.T) {
	base := openTestPool(test)
	ctx, cancel := context.WithTimeout(test.Context(), time.Minute)
	defer cancel()
	schema := createScratchSchema(test, ctx, base, "decision_park_")
	pool := openTestPoolWithSearchPath(test, schema)
	const full = `{"status":"in_progress","to":"need_decision","readiness_id":"prop-1","readiness":"ready","set":"gate"}`
	const bare = `{"status":"todo","to":"blocked"}`
	for _, statement := range []string{
		`CREATE TABLE issue (id uuid PRIMARY KEY, workspace_id uuid NOT NULL, metadata jsonb NOT NULL DEFAULT '{}')`,
		`INSERT INTO issue VALUES
		   ('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-0000000000aa', '{"keep":"me","decision_park":` + full + `}'),
		   ('00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-0000000000aa', '{"decision_park":` + bare + `}'),
		   ('00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-0000000000aa', '{"other":"value"}')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			test.Fatalf("fixture: %v", err)
		}
	}
	migrate := func(direction string) {
		test.Helper()
		if err := runMigrations(ctx, pool, runOptions{
			Direction: direction, Files: realMigrationFiles(test, []string{"985_issue_decision_park"}, direction),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks:                 hooksForDirection(direction), Conditions: conditionsForDirection(direction),
		}); err != nil {
			test.Fatalf("migrate %s: %v", direction, err)
		}
	}

	migrate("up")
	var parks, leftover int
	var priorStatus, toStatus string
	var readinessID *string
	var priorReadiness, setValue *string
	pool.QueryRow(ctx, `SELECT count(*) FROM issue_decision_park`).Scan(&parks)
	pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE metadata ? 'decision_park'`).Scan(&leftover)
	if parks != 2 || leftover != 0 {
		test.Fatalf("after up: %d park rows, %d issues still carry the key, want 2 and 0", parks, leftover)
	}
	pool.QueryRow(ctx, `SELECT prior_status, to_status, readiness_id, prior_readiness::text, set_value::text FROM issue_decision_park WHERE issue_id = '00000000-0000-0000-0000-000000000001'`).
		Scan(&priorStatus, &toStatus, &readinessID, &priorReadiness, &setValue)
	if priorStatus != "in_progress" || toStatus != "need_decision" || readinessID == nil || *readinessID != "prop-1" ||
		priorReadiness == nil || *priorReadiness != `"ready"` || setValue == nil || *setValue != `"gate"` {
		test.Fatalf("park row = %q %q %v %v %v", priorStatus, toStatus, readinessID, priorReadiness, setValue)
	}
	pool.QueryRow(ctx, `SELECT readiness_id, prior_readiness::text, set_value::text FROM issue_decision_park WHERE issue_id = '00000000-0000-0000-0000-000000000002'`).
		Scan(&readinessID, &priorReadiness, &setValue)
	if readinessID != nil || priorReadiness != nil || setValue != nil {
		test.Fatalf("bare park kept optional fields: %v %v %v", readinessID, priorReadiness, setValue)
	}
	var kept string
	pool.QueryRow(ctx, `SELECT metadata ->> 'keep' FROM issue WHERE id = '00000000-0000-0000-0000-000000000001'`).Scan(&kept)
	if kept != "me" {
		test.Fatalf("other metadata keys lost: keep = %q", kept)
	}

	migrate("down")
	var tableLeft bool
	pool.QueryRow(ctx, `SELECT to_regclass('issue_decision_park') IS NOT NULL`).Scan(&tableLeft)
	if tableLeft {
		test.Fatal("down left the table")
	}
	for id, want := range map[string]string{
		"00000000-0000-0000-0000-000000000001": full,
		"00000000-0000-0000-0000-000000000002": bare,
	} {
		var got []byte
		pool.QueryRow(ctx, `SELECT (metadata -> 'decision_park')::text FROM issue WHERE id = $1`, id).Scan(&got)
		var gotValue, wantValue any
		if json.Unmarshal(got, &gotValue) != nil || json.Unmarshal([]byte(want), &wantValue) != nil || !jsonEqual(gotValue, wantValue) {
			test.Fatalf("down restored %s = %s, want %s", id, got, want)
		}
	}
	var untouched int
	pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE id = '00000000-0000-0000-0000-000000000003' AND NOT (metadata ? 'decision_park')`).Scan(&untouched)
	if untouched != 1 {
		test.Fatal("down touched an issue that was never parked")
	}
}

func jsonEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
