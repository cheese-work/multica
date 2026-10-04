package main

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	wakeupDefinitionTableVersion = "580_wakeup_definition"
	wakeupInstanceOriginVersion  = "581_wakeup_instance_origin"
	wakeupDefinitionIndexVersion = "582_wakeup_definition_identity_index"
	wakeupDefinitionIndexName    = "issue_wakeup_definition_identity_idx"
)

// TestWakeupDefinitionIndexMigrationIsOneConcurrentStatement keeps the
// definition identity index in its own file so the runner can execute the
// CONCURRENTLY build outside a transaction, and keeps the file free of FKs.
func TestWakeupDefinitionIndexMigrationIsOneConcurrentStatement(t *testing.T) {
	t.Parallel()
	up := stripSQLComments(readWakeupMigration(t, wakeupDefinitionIndexVersion, "up"))
	if got := strings.Count(up, ";"); got != 1 {
		t.Fatalf("up migration has %d statements, want exactly 1:\n%s", got, up)
	}
	if !strings.Contains(up, "CREATE UNIQUE INDEX CONCURRENTLY") || !strings.Contains(up, wakeupDefinitionIndexName) {
		t.Fatalf("up migration must build %s concurrently:\n%s", wakeupDefinitionIndexName, up)
	}
	down := readWakeupMigration(t, wakeupDefinitionIndexVersion, "down")
	if !strings.Contains(down, "DROP INDEX CONCURRENTLY IF EXISTS "+wakeupDefinitionIndexName) || strings.Count(down, ";") != 1 {
		t.Fatalf("down migration must be one concurrent drop:\n%s", down)
	}
	// Table and column migrations create no index (not even a PK or UNIQUE
	// constraint) and no FK or cascade; the one index lives in its own file.
	for _, version := range []string{wakeupDefinitionTableVersion, wakeupInstanceOriginVersion} {
		for _, direction := range []string{"up", "down"} {
			body := strings.ToUpper(stripSQLComments(readWakeupMigration(t, version, direction)))
			for _, banned := range []string{"REFERENCES", "FOREIGN KEY", "ON DELETE", "ON UPDATE", "PRIMARY KEY", "UNIQUE", "INDEX"} {
				if strings.Contains(body, banned) {
					t.Errorf("%s.%s contains %q", version, direction, banned)
				}
			}
		}
	}
	if _, ok := concurrentIndexCleanups[wakeupDefinitionIndexVersion]; !ok {
		t.Errorf("%s has no invalid-index cleanup hook", wakeupDefinitionIndexVersion)
	}
	if _, ok := concurrentDownIndexCleanups[wakeupDefinitionIndexVersion]; ok {
		t.Errorf("%s down only drops; it must not register a rebuild cleanup", wakeupDefinitionIndexVersion)
	}
	req, ok := requiredConcurrentIndexes[wakeupDefinitionIndexVersion]
	if !ok || !req.Unique || strings.Join(req.Columns, ",") != "workspace_id,scope_kind,scope_id,rule_key" {
		t.Errorf("%s must require the unique identity index to be valid, got %+v", wakeupDefinitionIndexVersion, req)
	}
}

func stripSQLComments(sql string) string {
	var kept []string
	for _, line := range strings.Split(sql, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func readWakeupMigration(t *testing.T, version, direction string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", version+"."+direction+".sql"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWakeupDefinitionTableAndInstanceOriginUpDownUp(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "wakeup_definition_")
	pool := openTestPoolWithSearchPath(t, schema)
	if _, err := pool.Exec(ctx, `CREATE TABLE issue_wakeup (id UUID NOT NULL, issue_id UUID NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO issue_wakeup (id, issue_id) VALUES (gen_random_uuid(), gen_random_uuid())`); err != nil {
		t.Fatal(err)
	}
	versions := []string{wakeupDefinitionTableVersion, wakeupInstanceOriginVersion, wakeupDefinitionIndexVersion}
	count := func(query string) int {
		var n int
		if err := pool.QueryRow(ctx, query, schema).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var kept int
	for _, direction := range []string{"up", "down", "up"} {
		vs := versions
		if direction == "down" {
			vs = []string{wakeupDefinitionIndexVersion, wakeupInstanceOriginVersion, wakeupDefinitionTableVersion}
		}
		if err := runMigrations(ctx, pool, runOptions{
			Direction:             direction,
			Files:                 realMigrationFiles(t, vs, direction),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks:                 hooksForDirection(direction),
			Conditions:            conditionsForDirection(direction),
		}); err != nil {
			t.Fatalf("migrate %s: %v", direction, err)
		}
		want := map[string]int{"up": 1, "down": 0}[direction]
		if got := count(`SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name='issue_wakeup_definition'`); got != want {
			t.Fatalf("after %s, definition tables = %d, want %d", direction, got, want)
		}
		wantCols := map[string]int{"up": 4, "down": 0}[direction]
		if got := count(`SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='issue_wakeup' AND column_name IN ('default_rule_key','default_scope_kind','default_scope_id','config_fingerprint')`); got != wantCols {
			t.Fatalf("after %s, origin columns = %d, want %d", direction, got, wantCols)
		}
		if direction == "up" {
			assertIndexValidity(t, pool, schema, wakeupDefinitionIndexName, true)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue_wakeup WHERE default_rule_key IS NULL AND config_fingerprint IS NULL`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatalf("existing rows must keep NULL origin metadata, got %d", kept)
	}
}

func TestRunMigrationsRepairsInvalidWakeupDefinitionIndexBeforeRetry(t *testing.T) {
	t.Parallel()
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "wakeup_definition_idx_")
	pool := openTestPoolWithSearchPath(t, schema)
	if preMigrationHooks[wakeupDefinitionIndexVersion] == nil {
		t.Fatalf("production hook is not registered for %s", wakeupDefinitionIndexVersion)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE issue_wakeup (id UUID NOT NULL, issue_id UUID NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(ctx, pool, runOptions{
		Direction:             "up",
		Files:                 realMigrationFiles(t, []string{wakeupDefinitionTableVersion, wakeupInstanceOriginVersion}, "up"),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
	}); err != nil {
		t.Fatal(err)
	}
	const insert = `INSERT INTO issue_wakeup_definition (workspace_id, scope_kind, scope_id, rule_key, config)
		VALUES ('00000000-0000-7000-8000-000000000001','workspace','00000000-0000-7000-8000-000000000001','child_done','{"v":1}')`
	if _, err := pool.Exec(ctx, insert); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY `+wakeupDefinitionIndexName+
		` ON issue_wakeup_definition(workspace_id, scope_kind, scope_id, rule_key)`); err == nil {
		t.Fatal("initial unique build unexpectedly succeeded with duplicate identities")
	}
	assertIndexValidity(t, pool, schema, wakeupDefinitionIndexName, false)
	if _, err := pool.Exec(ctx, `DELETE FROM issue_wakeup_definition WHERE ctid=(SELECT max(ctid) FROM issue_wakeup_definition)`); err != nil {
		t.Fatal(err)
	}
	if err := runMigrations(ctx, pool, runOptions{
		Direction:             "up",
		Files:                 realMigrationFiles(t, []string{wakeupDefinitionIndexVersion}, "up"),
		SchemaMigrationsTable: schema + ".schema_migrations",
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 map[string]preMigrationHook{wakeupDefinitionIndexVersion: preMigrationHooks[wakeupDefinitionIndexVersion]},
	}); err != nil {
		t.Fatalf("retry after invalid index: %v", err)
	}
	assertIndexValidity(t, pool, schema, wakeupDefinitionIndexName, true)
}
