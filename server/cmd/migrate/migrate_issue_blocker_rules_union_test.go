package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// forkSystemRules are the issue_wakeup.system_rule values the fork writes
// (999d_pr_merge_owed_wakeup.up.sql). Upstream 746 narrows the check to
// ('child_done','blockers_done'); that fails ADD CONSTRAINT on any fork row
// that carries one of the other values, so the fork's 99zzz86 keeps the union.
var forkSystemRules = []string{
	"child_done", "pr_merged", "pr_checks_failed", "pr_needs_attention",
	"pr_checks_passed", "workflow_edges", "pr_dequeued", "pr_merge_owed",
}

const issueBlockerRulesVersion = "99zzz86_issue_blocker_rules"

// TestIssueBlockerRulesKeepsForkSystemRules (CHE-9432, slice C2c3) seeds one
// issue_wakeup row per fork system_rule value under the fork's 8-value check,
// then runs the renumbered upstream 746. Up must keep every row and accept
// blockers_done. Down must delete the blockers_done rows (data lost on down),
// keep the 8 fork rows, and restore the 8-value fork check.
func TestIssueBlockerRulesKeepsForkSystemRules(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, schema := newForkWakeupSchema(ctx, t)

	for i, rule := range forkSystemRules {
		if _, err := pool.Exec(ctx,
			`INSERT INTO issue_wakeup (id, workspace_id, issue_id, system_rule) VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), $1)`,
			rule); err != nil {
			t.Fatalf("seed fork row %d (%s): %v", i, rule, err)
		}
	}

	runDir := func(dir string) error {
		return runMigrations(ctx, pool, runOptions{
			Direction:             dir,
			Files:                 realMigrationFiles(t, []string{issueBlockerRulesVersion}, dir),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks:                 hooksForDirection(dir),
		})
	}

	// UP: every fork row survives, blockers_done becomes writable.
	if err := runDir("up"); err != nil {
		t.Fatalf("up %s with one row per fork system_rule: %v", issueBlockerRulesVersion, err)
	}
	if got := countWakeups(ctx, t, pool, ""); got != len(forkSystemRules) {
		t.Fatalf("after up: %d fork rows, want %d", got, len(forkSystemRules))
	}
	for _, rule := range forkSystemRules {
		if got := countWakeups(ctx, t, pool, rule); got != 1 {
			t.Fatalf("after up: %d rows for %q, want 1", got, rule)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO issue_wakeup (id, workspace_id, issue_id, system_rule) VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'blockers_done')`); err != nil {
		t.Fatalf("after up: blockers_done must be writable: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO issue_wakeup (id, workspace_id, issue_id, system_rule) VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'not_a_rule')`); err == nil {
		t.Fatalf("after up: an unknown system_rule must still be rejected")
	}

	// DOWN: blockers_done rows are deleted (data lost on down); the 8 fork
	// rows stay; the 8-value fork check is back.
	if err := runDir("down"); err != nil {
		t.Fatalf("down %s with fork rows and a blockers_done row: %v", issueBlockerRulesVersion, err)
	}
	if got := countWakeups(ctx, t, pool, "blockers_done"); got != 0 {
		t.Fatalf("after down: %d blockers_done rows, want 0", got)
	}
	if got := countWakeups(ctx, t, pool, ""); got != len(forkSystemRules) {
		t.Fatalf("after down: %d rows, want the %d fork rows", got, len(forkSystemRules))
	}
	for _, rule := range forkSystemRules {
		if _, err := pool.Exec(ctx,
			`INSERT INTO issue_wakeup (id, workspace_id, issue_id, system_rule) VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), $1)`,
			rule); err != nil {
			t.Fatalf("after down: fork rule %q must be writable: %v", rule, err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO issue_wakeup (id, workspace_id, issue_id, system_rule) VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'blockers_done')`); err == nil {
		t.Fatalf("after down: blockers_done must be rejected again")
	}
}

func countWakeups(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rule string) int {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM issue_wakeup`
	args := []any{}
	if rule != "" {
		q += ` WHERE system_rule = $1`
		args = append(args, rule)
	}
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count issue_wakeup: %v", err)
	}
	return n
}

// newForkWakeupSchema builds the smallest schema 746 needs, with issue_wakeup
// under the fork's check from 999d (8 values), in a private schema.
func newForkWakeupSchema(ctx context.Context, t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	adminPool := openTestPool(t)
	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
	schema := "migrate_blocker_rules_" + suffix
	schemaIdent := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+schemaIdent); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+schemaIdent+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})

	pool := openTestPoolWithSearchPath(t, schema)
	for _, stmt := range []string{
		`CREATE TABLE schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE issue (
			id UUID PRIMARY KEY,
			workspace_id UUID NOT NULL,
			parent_issue_id UUID,
			stage INTEGER,
			status TEXT NOT NULL DEFAULT 'todo'
		)`,
		`CREATE TABLE issue_status (
			workspace_id UUID NOT NULL,
			key TEXT NOT NULL,
			category TEXT NOT NULL
		)`,
		`CREATE TABLE issue_blocker (
			workspace_id UUID NOT NULL,
			issue_id UUID NOT NULL,
			blocker_issue_id UUID NOT NULL
		)`,
		`CREATE TABLE issue_wakeup (
			id UUID PRIMARY KEY,
			workspace_id UUID NOT NULL,
			issue_id UUID NOT NULL,
			system_rule TEXT
		)`,
		`CREATE TABLE issue_wakeup_receipt (
			wakeup_id UUID NOT NULL
		)`,
		// The fork's check, exactly as 999d_pr_merge_owed_wakeup.up.sql sets it.
		`ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_system_rule_check
			CHECK (system_rule IN ('child_done', 'pr_merged', 'pr_checks_failed', 'pr_needs_attention', 'pr_checks_passed', 'workflow_edges', 'pr_dequeued', 'pr_merge_owed'))`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	return pool, schema
}
