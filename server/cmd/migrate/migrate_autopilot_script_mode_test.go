package main

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const autopilotScriptModeVersion = "99976_autopilot_script_mode"

// CHE-5090: the execution_mode CHECK allows "script" and script_config exists
// after up. Down refuses while a script autopilot exists, then reverses clean;
// a second up works.
func TestAutopilotScriptModeMigrationUpDownUp(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "autopilot_script_")
	pool := openTestPoolWithSearchPath(t, schema)

	// The 042 shape of the table, reduced to the column under test.
	if _, err := pool.Exec(ctx, `CREATE TABLE autopilot (
		id SERIAL PRIMARY KEY,
		execution_mode TEXT NOT NULL DEFAULT 'create_issue'
			CHECK (execution_mode IN ('create_issue', 'run_only'))
	)`); err != nil {
		t.Fatalf("create autopilot: %v", err)
	}

	run := func(direction string) {
		t.Helper()
		if err := runMigrations(ctx, pool, runOptions{
			Direction:             direction,
			Files:                 realMigrationFiles(t, []string{autopilotScriptModeVersion}, direction),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks:                 hooksForDirection(direction),
			Conditions:            conditionsForDirection(direction),
		}); err != nil {
			t.Fatalf("migrate %s: %v", direction, err)
		}
	}
	insertScript := func() error {
		_, err := pool.Exec(ctx, `INSERT INTO autopilot (execution_mode, script_config) VALUES ('script', '{"name":"x","args":[]}')`)
		return err
	}

	if _, err := pool.Exec(ctx, `INSERT INTO autopilot (execution_mode) VALUES ('script')`); err == nil {
		t.Fatal("script accepted before up")
	}
	run("up")
	if err := insertScript(); err != nil {
		t.Fatalf("script rejected after up: %v", err)
	}

	// The down file refuses while a script autopilot exists.
	downSQL, err := os.ReadFile(filepath.Join("..", "..", "migrations", autopilotScriptModeVersion+".down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(downSQL)); err == nil {
		t.Fatal("down ran with a script autopilot present")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM autopilot WHERE execution_mode = 'script'`); err != nil {
		t.Fatal(err)
	}

	run("down")
	if err := insertScript(); err == nil {
		t.Fatal("script accepted after down")
	}
	run("up")
	if err := insertScript(); err != nil {
		t.Fatalf("script rejected after second up: %v", err)
	}
}
