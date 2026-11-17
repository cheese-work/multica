package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CHE-4273 (S1 of CHE-4265): storage for the CI failure watcher.
const ciWatchTablesVersion = "993_ci_watch_tables"

// version -> index, in migration order. Each is one concurrent unique build.
var ciWatchUniqueIndexes = []struct{ version, index, table string }{
	{"994_ci_watch_config_repo_uidx", "ci_watch_config_repo_uidx", "ci_watch_config"},
	{"995_ci_failure_event_run_attempt_uidx", "ci_failure_event_run_attempt_uidx", "ci_failure_event"},
	{"996_ci_watch_action_key_uidx", "ci_watch_action_key_uidx", "ci_watch_action"},
	{"997_ci_signature_owner_signature_uidx", "ci_signature_owner_signature_uidx", "ci_signature_owner"},
	{"998_ci_signature_owner_issue_uidx", "ci_signature_owner_issue_uidx", "ci_signature_owner"},
}

// 999 serves the 30-day prune of ci_failure_event.
const ciFailureEventPruneIndexVersion = "999_ci_failure_event_created_at_idx"

func TestCIWatchMigrationsFollowTheRules(t *testing.T) {
	t.Parallel()
	for _, direction := range []string{"up", "down"} {
		body := strings.ToUpper(stripSQLComments(readWakeupMigration(t, ciWatchTablesVersion, direction)))
		for _, banned := range []string{"REFERENCES", "FOREIGN KEY", "ON DELETE", "ON UPDATE", "CREATE INDEX", "CREATE UNIQUE", "UNIQUE ("} {
			if strings.Contains(body, banned) {
				t.Errorf("%s.%s contains %q", ciWatchTablesVersion, direction, banned)
			}
		}
	}
	for _, ix := range ciWatchUniqueIndexes {
		if got := concurrentIndexCleanups[ix.version]; got != ix.index {
			t.Errorf("%s cleanup index = %q, want %q", ix.version, got, ix.index)
		}
		req, ok := requiredConcurrentIndexes[ix.version]
		if !ok || req.IndexRegclass != ix.index || req.TableRegclass != ix.table || !req.Unique {
			t.Errorf("%s validity requirement = %+v", ix.version, req)
		}
		up := stripSQLComments(readWakeupMigration(t, ix.version, "up"))
		if strings.Count(up, ";") != 1 || !strings.Contains(up, "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "+ix.index) {
			t.Errorf("%s up must be one concurrent re-runnable build:\n%s", ix.version, up)
		}
		down := stripSQLComments(readWakeupMigration(t, ix.version, "down"))
		if strings.Count(down, ";") != 1 || !strings.Contains(down, "DROP INDEX CONCURRENTLY IF EXISTS "+ix.index) {
			t.Errorf("%s down must be one concurrent drop:\n%s", ix.version, down)
		}
	}
}

func TestCIFailureEventPruneIndexIsRecoverable(t *testing.T) {
	t.Parallel()
	const index = "ci_failure_event_created_at_idx"
	if got := concurrentIndexCleanups[ciFailureEventPruneIndexVersion]; got != index {
		t.Errorf("cleanup index = %q, want %q", got, index)
	}
	req := requiredConcurrentIndexes[ciFailureEventPruneIndexVersion]
	if req.IndexRegclass != index || req.TableRegclass != "ci_failure_event" || req.Unique || !equalIndexColumns(req.Columns, []string{"created_at"}) {
		t.Errorf("requirement = %+v", req)
	}
	up := stripSQLComments(readWakeupMigration(t, ciFailureEventPruneIndexVersion, "up"))
	if strings.Count(up, ";") != 1 || !strings.Contains(up, "CREATE INDEX CONCURRENTLY IF NOT EXISTS "+index) {
		t.Errorf("up must be one concurrent re-runnable build:\n%s", up)
	}
}

func TestCIWatchSchemaUpDownUpAndUniqueKeys(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "ci_watch_")
	pool := openTestPoolWithSearchPath(t, schema)

	// The shape migration 936 leaves behind; only the kind check matters here.
	// Its triggers need the decision_answer columns they read.
	if _, err := pool.Exec(ctx, `CREATE TABLE decision_answer (decision_request_id uuid, chosen_option_index integer, created_at timestamptz, disclaimed_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, readWakeupMigration(t, "936_system1_observation", "up")); err != nil {
		t.Fatal(err)
	}
	apply := func(version, direction string) {
		t.Helper()
		if _, err := pool.Exec(ctx, readWakeupMigration(t, version, direction)); err != nil {
			t.Fatalf("%s %s: %v", version, direction, err)
		}
	}
	up := func() {
		apply(ciWatchTablesVersion, "up")
		for _, ix := range ciWatchUniqueIndexes {
			apply(ix.version, "up")
		}
		apply(ciFailureEventPruneIndexVersion, "up")
	}
	down := func() {
		apply(ciFailureEventPruneIndexVersion, "down")
		for i := len(ciWatchUniqueIndexes) - 1; i >= 0; i-- {
			apply(ciWatchUniqueIndexes[i].version, "down")
		}
		apply(ciWatchTablesVersion, "down")
	}
	tables := func() int {
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = current_schema() AND (tablename LIKE 'ci\_%' OR tablename LIKE 'gh\_%budget%' OR tablename LIKE 'gh\_budget%')`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	up()
	if got := tables(); got != 7 {
		t.Fatalf("tables after up = %d, want 7", got)
	}
	// Re-running every up file is harmless (the runner records after running).
	for _, ix := range ciWatchUniqueIndexes {
		apply(ix.version, "up")
	}

	const ws = "11111111-1111-1111-1111-111111111111"
	const issue = "22222222-2222-2222-2222-222222222222"
	dup := func(name, first, second string) {
		t.Helper()
		if _, err := pool.Exec(ctx, first); err != nil {
			t.Fatalf("%s: first insert: %v", name, err)
		}
		_, err := pool.Exec(ctx, second)
		if err == nil || !strings.Contains(err.Error(), "23505") {
			t.Errorf("%s: duplicate insert error = %v, want unique violation", name, err)
		}
	}
	dup("ci_watch_config",
		`INSERT INTO ci_watch_config (workspace_id, repo_owner, repo_name) VALUES ('`+ws+`', 'o', 'r')`,
		`INSERT INTO ci_watch_config (workspace_id, repo_owner, repo_name) VALUES ('`+ws+`', 'o', 'r')`)
	dup("ci_failure_event",
		`INSERT INTO ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt) VALUES ('`+ws+`', 'o', 'r', 7, 1)`,
		`INSERT INTO ci_failure_event (workspace_id, repo_owner, repo_name, run_id, run_attempt) VALUES ('`+ws+`', 'o', 'r', 7, 1)`)
	dup("ci_watch_action",
		`INSERT INTO ci_watch_action (workspace_id, repo_owner, repo_name, original_run_id, action_kind) VALUES ('`+ws+`', 'o', 'r', 7, 'rerun')`,
		`INSERT INTO ci_watch_action (workspace_id, repo_owner, repo_name, original_run_id, action_kind) VALUES ('`+ws+`', 'o', 'r', 7, 'rerun')`)
	dup("ci_signature_owner signature",
		`INSERT INTO ci_signature_owner (workspace_id, repo_owner, repo_name, signature_hash) VALUES ('`+ws+`', 'o', 'r', 'abc')`,
		`INSERT INTO ci_signature_owner (workspace_id, repo_owner, repo_name, signature_hash) VALUES ('`+ws+`', 'o', 'r', 'abc')`)
	dup("ci_signature_owner issue",
		`INSERT INTO ci_signature_owner (workspace_id, repo_owner, repo_name, signature_hash, issue_id) VALUES ('`+ws+`', 'o', 'r', 'd1', '`+issue+`')`,
		`INSERT INTO ci_signature_owner (workspace_id, repo_owner, repo_name, signature_hash, issue_id) VALUES ('`+ws+`', 'o', 'r', 'd2', '`+issue+`')`)
	// A signature owner row without an issue yet (state creating) may repeat issue_id NULL.
	if _, err := pool.Exec(ctx, `INSERT INTO ci_signature_owner (workspace_id, repo_owner, repo_name, signature_hash) VALUES ('`+ws+`', 'o', 'r', 'n1'), ('`+ws+`', 'o', 'r', 'n2')`); err != nil {
		t.Errorf("two creating owners without an issue must coexist: %v", err)
	}
	dup("gh_installation_budget",
		`INSERT INTO gh_installation_budget (installation_id, remaining) VALUES (5, 100)`,
		`INSERT INTO gh_installation_budget (installation_id, remaining) VALUES (5, 100)`)
	if _, err := pool.Exec(ctx, `INSERT INTO gh_budget_reservation (installation_id, cost, expires_at) VALUES (5, 1, now())`); err != nil {
		t.Fatal(err)
	}

	// The audit table and the defaults the panel relies on.
	if _, err := pool.Exec(ctx, `INSERT INTO ci_watch_config_audit (workspace_id, config_id, actor_id, field, old_value, new_value) VALUES ('`+ws+`', gen_random_uuid(), gen_random_uuid(), 'mode', 'off', 'shadow')`); err != nil {
		t.Fatal(err)
	}
	var mode, p1 string
	if err := pool.QueryRow(ctx, `SELECT mode, p1_mode FROM ci_watch_config`).Scan(&mode, &p1); err != nil || mode != "off" || p1 != "shadow" {
		t.Errorf("config defaults = %q/%q (err %v), want off/shadow", mode, p1, err)
	}

	// Caps follow the parent plan: reruns per original run, new issues per day, owner wakes per hour.
	var reruns, issues, wakes int
	if err := pool.QueryRow(ctx, `SELECT max_reruns_per_original_run, max_new_issues_per_day, max_owner_wakes_per_hour FROM ci_watch_config`).Scan(&reruns, &issues, &wakes); err != nil || reruns != 1 || issues != 5 || wakes != 6 {
		t.Errorf("cap defaults = %d/%d/%d (err %v), want 1/5/6", reruns, issues, wakes, err)
	}

	// system1_observation accepts the new kind and still refuses others.
	insertObs := func(kind string) error {
		_, err := pool.Exec(ctx, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class) VALUES ('`+ws+`', '`+kind+`', gen_random_uuid(), 'x')`)
		return err
	}
	if err := insertObs("ci_event"); err != nil {
		t.Errorf("ci_event observation: %v", err)
	}
	if err := insertObs("bogus"); err == nil {
		t.Error("kind check must still refuse unknown kinds")
	}

	// Down removes everything (including ci_event rows) and restores the old check; up again works.
	down()
	if got := tables(); got != 0 {
		t.Fatalf("tables after down = %d, want 0", got)
	}
	if err := insertObs("ci_event"); err == nil {
		t.Error("ci_event must be refused after down")
	}
	up()
	if got := tables(); got != 7 {
		t.Fatalf("tables after up-down-up = %d, want 7", got)
	}
}

// CHE-4279 (S7): one P4 notification per (repo, signature, day).
func TestCIWatchEventNotifyIndexIsOneConcurrentBuild(t *testing.T) {
	t.Parallel()
	const version, index = "999c_ci_watch_event_notify_uidx", "ci_watch_event_notify_uidx"
	if got := concurrentIndexCleanups[version]; got != index {
		t.Errorf("cleanup index = %q, want %q", got, index)
	}
	req := requiredConcurrentIndexes[version]
	if req.IndexRegclass != index || req.TableRegclass != "ci_watch_event" || !req.Unique || req.Predicate == "" {
		t.Errorf("requirement = %+v", req)
	}
	up := stripSQLComments(readWakeupMigration(t, version, "up"))
	if strings.Count(up, ";") != 1 || !strings.Contains(up, "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "+index) {
		t.Errorf("up must be one concurrent re-runnable build:\n%s", up)
	}
	down := stripSQLComments(readWakeupMigration(t, version, "down"))
	if strings.Count(down, ";") != 1 || !strings.Contains(down, "DROP INDEX CONCURRENTLY IF EXISTS "+index) {
		t.Errorf("down must be one concurrent drop:\n%s", down)
	}
}
