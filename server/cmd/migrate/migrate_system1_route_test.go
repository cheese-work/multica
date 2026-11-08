package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CHE-4548 (S1 of CHE-4439): system1_observation gains kind `route` and a
// nullable event_id, plus a non-unique partial index on the route rows.
const (
	system1RouteColumnVersion = "99952_system1_observation_route_event_id"
	system1RouteIndexVersion  = "99953_system1_observation_route_event_id_idx"
	system1RouteIndexName     = "system1_observation_route_event_idx"
)

func TestSystem1RouteIndexMigrationFollowsTheRules(t *testing.T) {
	t.Parallel()
	if got := concurrentIndexCleanups[system1RouteIndexVersion]; got != system1RouteIndexName {
		t.Errorf("cleanup index = %q, want %q", got, system1RouteIndexName)
	}
	req := requiredConcurrentIndexes[system1RouteIndexVersion]
	if req.IndexRegclass != system1RouteIndexName || req.TableRegclass != "system1_observation" || req.Unique ||
		!equalIndexColumns(req.Columns, []string{"workspace_id", "event_id"}) || req.Predicate == "" {
		t.Errorf("requirement = %+v", req)
	}
	up := stripSQLComments(readWakeupMigration(t, system1RouteIndexVersion, "up"))
	if strings.Count(up, ";") != 1 || !strings.Contains(up, "CREATE INDEX CONCURRENTLY IF NOT EXISTS "+system1RouteIndexName) {
		t.Errorf("up must be one concurrent re-runnable build:\n%s", up)
	}
	down := stripSQLComments(readWakeupMigration(t, system1RouteIndexVersion, "down"))
	if strings.Count(down, ";") != 1 || !strings.Contains(down, "DROP INDEX CONCURRENTLY IF EXISTS "+system1RouteIndexName) {
		t.Errorf("down must be one concurrent drop:\n%s", down)
	}
	// The index file must sort after the column file or the build has no column.
	if system1RouteIndexVersion <= system1RouteColumnVersion {
		t.Errorf("%s must sort after %s", system1RouteIndexVersion, system1RouteColumnVersion)
	}
}

func TestSystem1RouteSchemaUpDownUp(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "system1_route_")
	pool := openTestPoolWithSearchPath(t, schema)

	// Migration 936 leaves the table; 993 widens the kind check to ci_event.
	// 936's triggers read decision_answer columns.
	if _, err := pool.Exec(ctx, `CREATE TABLE decision_answer (decision_request_id uuid, chosen_option_index integer, created_at timestamptz, disclaimed_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	apply := func(version, direction string) {
		t.Helper()
		if _, err := pool.Exec(ctx, readWakeupMigration(t, version, direction)); err != nil {
			t.Fatalf("%s %s: %v", version, direction, err)
		}
	}
	apply("936_system1_observation", "up")
	apply("993_ci_watch_tables", "up")

	const ws = "11111111-1111-1111-1111-111111111111"
	insert := func(kind, eventID string) error {
		ev := "NULL"
		if eventID != "" {
			ev = "'" + eventID + "'"
		}
		_, err := pool.Exec(ctx, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class, event_id) VALUES ('`+ws+`', '`+kind+`', gen_random_uuid(), 'x', `+ev+`)`)
		return err
	}
	if err := insert("route", ""); err == nil {
		t.Fatal("before the migration: route (and event_id) must be refused")
	}

	apply(system1RouteColumnVersion, "up")
	apply(system1RouteIndexVersion, "up")
	// Re-running the index file is harmless.
	apply(system1RouteIndexVersion, "up")

	const ev = "33333333-3333-3333-3333-333333333333"
	for _, kind := range []string{"decision", "routing", "ci_event"} {
		if err := insert(kind, ""); err != nil {
			t.Errorf("%s must stay valid: %v", kind, err)
		}
	}
	if err := insert("route", ev); err != nil {
		t.Fatalf("route with event_id: %v", err)
	}
	// Not unique: a duplicate event id stays countable.
	if err := insert("route", ev); err != nil {
		t.Errorf("duplicate route event_id must be allowed: %v", err)
	}
	if err := insert("bogus", ""); err == nil {
		t.Error("kind check must still refuse unknown kinds")
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(to_regclass($1))`, system1RouteIndexName).Scan(&def); err != nil ||
		!strings.Contains(def, "(workspace_id, event_id)") || !strings.Contains(def, "route") || strings.Contains(def, "UNIQUE") {
		t.Errorf("index def = %q (err %v)", def, err)
	}

	// Down drops the index and column, deletes route rows, and restores the old check.
	apply(system1RouteIndexVersion, "down")
	apply(system1RouteColumnVersion, "down")
	var routes, others int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind = 'route'), count(*) FILTER (WHERE kind <> 'route') FROM system1_observation`).Scan(&routes, &others); err != nil {
		t.Fatal(err)
	}
	if routes != 0 || others != 3 {
		t.Errorf("after down route/other rows = %d/%d, want 0/3", routes, others)
	}
	var col int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'system1_observation' AND column_name = 'event_id'`).Scan(&col); err != nil || col != 0 {
		t.Errorf("event_id column after down = %d (err %v)", col, err)
	}
	// event_id is gone, so insert without it.
	if _, err := pool.Exec(ctx, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class) VALUES ('`+ws+`', 'ci_event', gen_random_uuid(), 'x')`); err != nil {
		t.Errorf("ci_event must survive the down: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO system1_observation (workspace_id, kind, subject_id, class) VALUES ('`+ws+`', 'route', gen_random_uuid(), 'x')`); err == nil {
		t.Error("route must be refused after down")
	}

	apply(system1RouteColumnVersion, "up")
	apply(system1RouteIndexVersion, "up")
	if err := insert("route", ev); err != nil {
		t.Errorf("route after up-down-up: %v", err)
	}
}
