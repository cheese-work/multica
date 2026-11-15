package main

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// chatAttentionVersions lists the migrations this layer owns, in order.
var chatAttentionVersions = []string{
	"970_chat_attention",
	"975_chat_session_chain_root_idx",
	"976_chat_attention_item_one_open_later_idx",
	"977_chat_attention_item_open_idx",
}

// versionsFor orders the versions as the runner would: up ascending, down descending.
func versionsFor(direction string) []string {
	v := append([]string(nil), chatAttentionVersions...)
	if direction == "down" {
		slices.Reverse(v)
	}
	return v
}

// Repo AGENTS.md "Database and Migration Rules": no foreign keys or cascades; every index is
// CREATE [UNIQUE] INDEX CONCURRENTLY in its own single-statement file.
func TestChatAttentionMigrationsFollowRepoRules(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "97[05-7]_*chat*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(chatAttentionVersions) {
		t.Errorf("want %d chat attention up files, got %v", len(chatAttentionVersions), files)
	}
	comment := regexp.MustCompile(`(?m)--.*$`)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sql := strings.ToUpper(comment.ReplaceAllString(string(raw), ""))
		if strings.Contains(sql, "REFERENCES") || strings.Contains(sql, "CASCADE") {
			t.Errorf("%s: foreign key or cascade", f)
		}
		if strings.Contains(sql, "INDEX") {
			if !strings.Contains(sql, "INDEX CONCURRENTLY") {
				t.Errorf("%s: index not built CONCURRENTLY", f)
			}
			if n := strings.Count(sql, ";"); n != 1 {
				t.Errorf("%s: want one statement, got %d", f, n)
			}
		}
	}
}

// CHE-4145: 970 back-fills chain_root_session_id from continued_from_session_id, defaults new
// rows to their own id, and reverses cleanly.
func TestChatAttentionMigrationBackfillTriggerAndDown(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "chat_attention_")
	pool := openTestPoolWithSearchPath(t, schema)
	for _, ddl := range []string{
		`CREATE TABLE workspace (id uuid PRIMARY KEY)`,
		`CREATE TABLE "user" (id uuid PRIMARY KEY)`,
		`CREATE TABLE chat_session (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), continued_from_session_id uuid, continued_into_session_id uuid)`,
		// A <- B <- C chain, standalone D, a cycle E <-> F, and G whose parent row is gone.
		`INSERT INTO chat_session (id, continued_from_session_id) VALUES
			('00000000-0000-0000-0000-00000000000a', NULL),
			('00000000-0000-0000-0000-00000000000b', '00000000-0000-0000-0000-00000000000a'),
			('00000000-0000-0000-0000-00000000000c', '00000000-0000-0000-0000-00000000000b'),
			('00000000-0000-0000-0000-00000000000d', NULL),
			('00000000-0000-0000-0000-00000000000e', '00000000-0000-0000-0000-00000000000f'),
			('00000000-0000-0000-0000-00000000000f', '00000000-0000-0000-0000-00000000000e'),
			('00000000-0000-0000-0000-000000000010', '00000000-0000-0000-0000-0000000000ff')`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	run := func(direction string) {
		t.Helper()
		if err := runMigrations(ctx, pool, runOptions{
			Direction:             direction,
			Files:                 realMigrationFiles(t, versionsFor(direction), direction),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks:                 hooksForDirection(direction),
			Conditions:            conditionsForDirection(direction),
		}); err != nil {
			t.Fatalf("migrate %s: %v", direction, err)
		}
	}

	run("up")
	roots := map[string]string{}
	rows, err := pool.Query(ctx, `SELECT right(id::text, 2), right(chain_root_session_id::text, 2) FROM chat_session`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, root *string
		if err := rows.Scan(&id, &root); err != nil {
			t.Fatal(err)
		}
		if root == nil {
			roots[*id] = "<null>"
			continue
		}
		roots[*id] = *root
	}
	rows.Close()
	want := map[string]string{"0a": "0a", "0b": "0a", "0c": "0a", "0d": "0d", "0e": "0e", "0f": "0f", "10": "10"}
	for id, w := range want {
		if roots[id] != w {
			t.Errorf("root of %s = %s, want %s", id, roots[id], w)
		}
	}

	var own bool
	if err := pool.QueryRow(ctx, `INSERT INTO chat_session DEFAULT VALUES RETURNING chain_root_session_id = id`).Scan(&own); err != nil || !own {
		t.Fatalf("new session root = own id: %v %v", own, err)
	}

	run("down")
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'chat_session' AND column_name = 'chain_root_session_id')`, schema).Scan(&exists); err != nil || exists {
		t.Fatalf("column after down exists=%v: %v", exists, err)
	}
	run("up") // re-runnable
}
