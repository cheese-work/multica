package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	wakeupScopedEventTableVersion     = "592_wakeup_scoped_event"
	wakeupScopedEventCaptureVersion   = "597_wakeup_scoped_event_capture"
	wakeupScopedEventConcurrentMigrat = "593_wakeup_scoped_event_id_index 594_wakeup_scoped_event_pending_index 595_wakeup_scoped_event_handled_index 596_wakeup_scoped_event_issue_index"
)

// The outbox migrations add a table, a column and functions only: no FK,
// cascade, PK or inline index. Each of its three indexes is one concurrent
// statement in its own file, with a cleanup hook and a validity requirement.
func TestWakeupScopedEventMigrationsFollowTheRules(t *testing.T) {
	t.Parallel()
	for _, version := range []string{wakeupScopedEventTableVersion, wakeupScopedEventCaptureVersion} {
		for _, direction := range []string{"up", "down"} {
			body := strings.ToUpper(stripSQLComments(readWakeupMigration(t, version, direction)))
			for _, banned := range []string{"REFERENCES", "FOREIGN KEY", "ON DELETE", "ON UPDATE", "CREATE INDEX", "CREATE UNIQUE", "PRIMARY KEY"} {
				if strings.Contains(body, banned) {
					t.Errorf("%s.%s contains %q", version, direction, banned)
				}
			}
		}
	}
	for _, version := range strings.Fields(wakeupScopedEventConcurrentMigrat) {
		index := concurrentIndexCleanups[version]
		if index == "" {
			t.Errorf("%s has no invalid-index cleanup hook", version)
			continue
		}
		up := stripSQLComments(readWakeupMigration(t, version, "up"))
		if strings.Count(up, ";") != 1 || !strings.Contains(up, "INDEX CONCURRENTLY") || !strings.Contains(up, index) || !strings.Contains(up, "IF NOT EXISTS") {
			t.Errorf("%s up must be one concurrent, re-runnable build of %s:\n%s", version, index, up)
		}
		down := readWakeupMigration(t, version, "down")
		if !strings.Contains(down, "DROP INDEX CONCURRENTLY IF EXISTS "+index) || strings.Count(down, ";") != 1 {
			t.Errorf("%s down must be one concurrent drop:\n%s", version, down)
		}
		if got := requiredConcurrentIndexes[version].IndexRegclass; got != index {
			t.Errorf("%s validity requirement = %q, want %q", version, got, index)
		}
	}
}

// The runner records a version after running its SQL, so an interruption in
// between makes the next run execute the same SQL again, and a rollback must
// return the capture functions as they were. Apply both migrations twice, roll
// back, apply again.
func TestWakeupScopedEventMigrationsAreRetrySafeAndReversible(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "wakeup_scoped_event_")
	pool := openTestPoolWithSearchPath(t, schema)
	// Stubs for the composite types the capture functions declare and the columns
	// the SQL-language chain function is checked against when it is created.
	for _, stmt := range []string{
		`CREATE TABLE issue_wakeup_definition (workspace_id uuid, scope_kind text, scope_id uuid, rule_key text, revision bigint, updated_at timestamptz, event_types text[])`,
		`CREATE TABLE issue_wakeup (id uuid)`,
		`CREATE TABLE comment (id uuid)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	apply := func(version, direction string) {
		t.Helper()
		if _, err := pool.Exec(ctx, readWakeupMigration(t, version, direction)); err != nil {
			t.Fatalf("%s %s: %v", version, direction, err)
		}
	}
	count := func(query string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	capture := func() int {
		return count(`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=current_schema() AND p.proname='capture_issue_wakeup'`)
	}
	probes := func() int {
		// two probes and the chain snapshot
		return count(`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=current_schema() AND p.proname LIKE 'wakeup_scoped_event_%'`)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		apply(wakeupScopedEventTableVersion, "up")
		apply(wakeupScopedEventCaptureVersion, "up")
	}
	if got := capture(); got != 1 {
		t.Fatalf("capture_issue_wakeup definitions after a retry = %d, want exactly one", got)
	}
	if got := probes(); got != 3 {
		t.Fatalf("probe functions = %d, want 3", got)
	}
	apply(wakeupScopedEventCaptureVersion, "down")
	if got := capture(); got != 1 || probes() != 0 {
		t.Fatalf("rollback left capture=%d probes=%d, want the pre-L9 function and no probes", got, probes())
	}
	if got := count(`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=current_schema() AND p.proname='capture_issue_wakeup' AND p.pronargs=6`); got != 1 {
		t.Fatalf("rollback must restore the six-argument capture_issue_wakeup, found %d", got)
	}
	apply(wakeupScopedEventTableVersion, "down")
	apply(wakeupScopedEventTableVersion, "up")
	apply(wakeupScopedEventCaptureVersion, "up")
	if got := capture(); got != 1 {
		t.Fatalf("capture_issue_wakeup definitions after up-down-up = %d, want one", got)
	}
}
