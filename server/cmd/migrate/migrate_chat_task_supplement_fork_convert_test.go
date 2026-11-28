package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CHE-7535 S1: the fork's chat_task_supplement (875/876) collides with the
// upstream table that 99zzz01 creates. 99zzz00 renames the fork table out of the
// way; a pre-migration hook on the 662 target copies the delivered rows.

const (
	supplementForkConvertVersion = "99zzz00_chat_task_supplement_fork_convert"
	supplementLastUpstreamStep   = "99zzz04_chat_task_supplement_workspace"
	supplementHookStem           = "662_chat_task_supplement_message"
)

func supplementScratchDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("supplement_s1_%d_%d", time.Now().UnixNano(), rand.Uint32())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	config, err := pgxpool.ParseConfig(testDatabaseURL())
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop scratch database %s: %v", name, err)
		}
	})
	return pool
}

// supplementUpFiles returns the real up files in apply order whose stem is
// >= from (empty: no lower bound) and <= through (empty: no upper bound).
func supplementUpFiles(t *testing.T, from, through string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	var picked []string
	for _, file := range files {
		stem := strings.TrimSuffix(filepath.Base(file), ".up.sql")
		if from != "" && stem < from {
			continue
		}
		if through != "" && stem > through {
			continue
		}
		picked = append(picked, file)
	}
	return picked
}

type supplementRun struct {
	pool  *pgxpool.Pool
	lock  int64
	hooks map[string]preMigrationHook
}

func newSupplementRun(pool *pgxpool.Pool) *supplementRun {
	return &supplementRun{pool: pool, lock: int64(rand.Uint64()&0x7fffffffffffffff) | 1, hooks: preMigrationHooks}
}

func (r *supplementRun) up(t *testing.T, files []string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return runMigrations(ctx, r.pool, runOptions{
		Direction:       "up",
		Files:           files,
		AdvisoryLockKey: r.lock,
		Hooks:           r.hooks,
		Conditions:      conditionsForDirection("up"),
	})
}

func (r *supplementRun) down(t *testing.T, stems ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var files []string
	for _, stem := range stems {
		files = append(files, filepath.Join("..", "..", "migrations", stem+".down.sql"))
	}
	return runMigrations(ctx, r.pool, runOptions{
		Direction:       "down",
		Files:           files,
		AdvisoryLockKey: r.lock,
		Hooks:           hooksForDirection("down"),
		Conditions:      conditionsForDirection("down"),
	})
}

type supplementSeedRow struct {
	message, task, workspace, status string
	deliveredAt                      string // empty: NULL
}

const (
	wsA = "00000000-0000-0000-0000-00000000a001"
	wsB = "00000000-0000-0000-0000-00000000b001"
)

func supplementID(kind string, n int) string {
	return fmt.Sprintf("00000000-0000-0000-%s-%012d", kind, n)
}

// supplementSeed covers every status, two delivered rows for one message with
// different delivered_at, two with the same delivered_at (task_id breaks the
// tie), and two workspaces. 10 rows: 4 copied, 2 duplicates, 4 not delivered.
func supplementSeed() []supplementSeedRow {
	m := func(n int) string { return supplementID("0000", n) }
	t0 := func(n int) string { return supplementID("0001", n) }
	return []supplementSeedRow{
		{m(1), t0(1), wsA, "delivered", "2026-01-01 00:00:10+00"},
		{m(1), t0(2), wsA, "delivered", "2026-01-01 00:00:05+00"}, // earliest: kept
		{m(2), t0(3), wsA, "delivered", "2026-01-01 00:01:00+00"},
		{m(3), t0(4), wsA, "pending", ""},
		{m(4), t0(5), wsA, "delivering", ""},
		{m(5), t0(6), wsA, "uncertain", ""},
		{m(6), t0(7), wsA, "failed", ""},
		{m(7), t0(8), wsB, "delivered", "2026-01-02 00:00:00+00"},
		{m(8), t0(10), wsB, "delivered", "2026-01-03 00:00:00+00"},
		{m(8), t0(9), wsB, "delivered", "2026-01-03 00:00:00+00"}, // tie: lowest task_id kept
	}
}

func seedForkSupplements(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, rows []supplementSeedRow) {
	t.Helper()
	for i, row := range rows {
		var delivered any
		if row.deliveredAt != "" {
			delivered = row.deliveredAt
		}
		_, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (message_id, task_id, workspace_id, chat_session_id, author_id, status, delivered_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7::timestamptz, '2026-01-01 00:00:00+00'::timestamptz + $8 * interval '1 second')`, table),
			row.message, row.task, row.workspace, supplementID("5555", i), supplementID("aaaa", i), row.status, delivered, i)
		if err != nil {
			t.Fatalf("seed fork supplement %d: %v", i, err)
		}
	}
}

func relationExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestSupplementForkConvertFreshInstallPassesUpstreamSteps(t *testing.T) {
	pool := supplementScratchDatabase(t)
	run := newSupplementRun(pool)
	if err := run.up(t, supplementUpFiles(t, "", supplementLastUpstreamStep)); err != nil {
		t.Fatalf("fresh install must pass 99zzz01-99zzz04: %v", err)
	}
	ctx := t.Context()
	for _, name := range []string{
		"chat_task_supplement_fork_875", "chat_task_supplement_fork_875_pkey", "chat_task_supplement_fork_875_task_status_idx",
		"chat_task_supplement", "uq_chat_task_supplement_message", "idx_chat_task_supplement_task", "idx_chat_task_supplement_workspace",
	} {
		if !relationExists(t, ctx, pool, name) {
			t.Errorf("relation %s missing after fresh install", name)
		}
	}
	for _, name := range []string{"chat_task_supplement_pkey", "chat_task_supplement_task_status_idx"} {
		if relationExists(t, ctx, pool, name) {
			t.Errorf("fork index name %s still taken after the rename", name)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_task_supplement`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("fresh install upstream rows = %d, %v; want 0", rows, err)
	}
}

func TestSupplementForkConvertUpgradeCopiesDeliveredRows(t *testing.T) {
	pool := supplementScratchDatabase(t)
	run := newSupplementRun(pool)
	ctx := t.Context()
	// Fork database: everything before S1 applied, 875/876 included.
	if err := run.up(t, supplementUpFiles(t, "", "99zzz")); err != nil {
		t.Fatalf("build fork baseline: %v", err)
	}
	if !relationExists(t, ctx, pool, "chat_task_supplement_task_status_idx") {
		t.Fatal("baseline is not a fork database: 876 index missing")
	}
	seed := supplementSeed()
	seedForkSupplements(t, ctx, pool, "chat_task_supplement", seed)
	forkBefore := forkSnapshot(t, ctx, pool, "chat_task_supplement")

	if err := run.up(t, supplementUpFiles(t, "", supplementLastUpstreamStep)); err != nil {
		t.Fatalf("fork upgrade must pass 99zzz01-99zzz04: %v", err)
	}

	type copied struct{ Message, Task, Workspace, Status string }
	rows, err := pool.Query(ctx, `SELECT message_id::text, task_id::text, workspace_id::text, status FROM chat_task_supplement ORDER BY message_id`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[copied])
	if err != nil {
		t.Fatal(err)
	}
	want := []copied{
		{supplementID("0000", 1), supplementID("0001", 2), wsA, "delivered"}, // earliest delivered_at
		{supplementID("0000", 2), supplementID("0001", 3), wsA, "delivered"},
		{supplementID("0000", 7), supplementID("0001", 8), wsB, "delivered"},
		{supplementID("0000", 8), supplementID("0001", 9), wsB, "delivered"}, // lowest task_id on a tie
	}
	if !slices.Equal(got, want) {
		t.Fatalf("copied rows:\n got  %v\n want %v", got, want)
	}

	var sessionOK int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM chat_task_supplement c
		JOIN chat_task_supplement_fork_875 f USING (message_id, task_id)
		WHERE c.chat_session_id = f.chat_session_id AND c.author_id = f.author_id AND c.created_at = f.created_at`).Scan(&sessionOK); err != nil || sessionOK != 4 {
		t.Fatalf("session, author and created_at preserved on %d of 4 rows: %v", sessionOK, err)
	}
	if after := forkSnapshot(t, ctx, pool, "chat_task_supplement_fork_875"); after != forkBefore {
		t.Fatalf("fork table changed by the conversion:\n before %s\n after  %s", forkBefore, after)
	}
}

func forkSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) string {
	t.Helper()
	var snapshot string
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) || ':' || coalesce(md5(string_agg(f::text, '|' ORDER BY message_id, task_id)), '') FROM %s f`, table)).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestSupplementForkCopyCountsAddUpToForkRows(t *testing.T) {
	pool := supplementScratchDatabase(t)
	run := newSupplementRun(pool)
	ctx := t.Context()
	if err := run.up(t, supplementUpFiles(t, "", "99zzz")); err != nil {
		t.Fatal(err)
	}
	seedForkSupplements(t, ctx, pool, "chat_task_supplement", supplementSeed())
	if err := run.up(t, supplementUpFiles(t, "", "99zzz01_chat_task_supplement")); err != nil {
		t.Fatalf("up through 99zzz01: %v", err)
	}
	counts, err := copyChatTaskSupplementFork(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	want := chatTaskSupplementForkCounts{Copied: 4, DroppedDuplicate: 2, DiscardedNotDelivered: 4, Inserted: 4}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	if sum := counts.Copied + counts.DroppedDuplicate + counts.DiscardedNotDelivered; sum != len(supplementSeed()) {
		t.Fatalf("counts sum to %d, fork has %d rows", sum, len(supplementSeed()))
	}
	again, err := copyChatTaskSupplementFork(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	want.Inserted = 0
	if again != want {
		t.Fatalf("rerun counts = %+v, want %+v (nothing inserted twice)", again, want)
	}
}

func TestSupplementForkCopyRerunAfterFailureAfterHook(t *testing.T) {
	pool := supplementScratchDatabase(t)
	run := newSupplementRun(pool)
	ctx := t.Context()
	if err := run.up(t, supplementUpFiles(t, "", "99zzz")); err != nil {
		t.Fatal(err)
	}
	seedForkSupplements(t, ctx, pool, "chat_task_supplement", supplementSeed())

	target := supplementHookTarget(t)
	realHook := preMigrationHooks[target]
	failures := 0
	run.hooks = map[string]preMigrationHook{}
	for version, hook := range preMigrationHooks {
		run.hooks[version] = hook
	}
	run.hooks[target] = func(ctx context.Context, pool *pgxpool.Pool) error {
		if err := realHook(ctx, pool); err != nil {
			return err
		}
		if failures == 0 {
			failures++
			return errors.New("injected failure after the copy hook")
		}
		return nil
	}
	files := supplementUpFiles(t, "", supplementLastUpstreamStep)
	if err := run.up(t, files); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("first run must stop after the hook, got %v", err)
	}
	var first string
	if err := pool.QueryRow(ctx, `SELECT count(*) || ':' || md5(string_agg(c::text, '|' ORDER BY message_id)) FROM chat_task_supplement c`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := run.up(t, files); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var second string
	if err := pool.QueryRow(ctx, `SELECT count(*) || ':' || md5(string_agg(c::text, '|' ORDER BY message_id)) FROM chat_task_supplement c`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(second, "4:") {
		t.Fatalf("rows after rerun = %s, after the failed run = %s; want the same 4 rows", second, first)
	}
}

func TestSupplementForkConvertDownRestoresForkTableAndUpAgain(t *testing.T) {
	pool := supplementScratchDatabase(t)
	run := newSupplementRun(pool)
	ctx := t.Context()
	files := supplementUpFiles(t, "", supplementLastUpstreamStep)
	if err := run.up(t, files); err != nil {
		t.Fatal(err)
	}
	if err := run.down(t,
		"99zzz04_chat_task_supplement_workspace", "99zzz03_chat_task_supplement_task", "99zzz02_chat_task_supplement_message",
		"99zzz01_chat_task_supplement", supplementForkConvertVersion); err != nil {
		t.Fatalf("down to the fork baseline: %v", err)
	}
	for _, name := range []string{"chat_task_supplement", "chat_task_supplement_pkey", "chat_task_supplement_task_status_idx"} {
		if !relationExists(t, ctx, pool, name) {
			t.Errorf("%s missing after down", name)
		}
	}
	if relationExists(t, ctx, pool, "chat_task_supplement_fork_875") {
		t.Error("renamed fork table still present after down")
	}
	var nullable string
	if err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_name = 'task_supplement_capability' AND column_name = 'issue_id'`).Scan(&nullable); err != nil || nullable != "YES" {
		t.Fatalf("task_supplement_capability.issue_id nullable = %q, %v; the fork 875 state is nullable", nullable, err)
	}
	if err := run.up(t, files); err != nil {
		t.Fatalf("up again after down: %v", err)
	}
}

func supplementHookTarget(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("renumber-map.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct{ From, To string }
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.From == supplementHookStem {
			return entry.To
		}
	}
	t.Fatalf("renumber-map.json has no entry from %s", supplementHookStem)
	return ""
}

// The registry scan reads no <digits><letters><digits>_ key, so this test is the
// only guard of the hook key: it follows the map, not a literal.
func TestSupplementForkCopyHookIsRegisteredOnTheMapTarget(t *testing.T) {
	target := supplementHookTarget(t)
	if _, ok := forkDataCopyHooks[target]; !ok || len(forkDataCopyHooks) != 1 {
		t.Fatalf("forkDataCopyHooks must hold exactly the map target %s, has %v", target, forkDataCopyHooks)
	}
	hook := preMigrationHooks[target]
	if hook == nil {
		t.Fatalf("no pre-migration hook on %s", target)
	}

	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, admin, "supplement_hook_")
	pool := openTestPoolWithSearchPath(t, schema)
	for _, statement := range []string{
		`CREATE TABLE chat_task_supplement_fork_875 (
			message_id UUID NOT NULL, task_id UUID NOT NULL, workspace_id UUID NOT NULL, chat_session_id UUID NOT NULL,
			author_id UUID NOT NULL, status TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), delivered_at TIMESTAMPTZ,
			PRIMARY KEY (message_id, task_id))`,
		`CREATE TABLE chat_task_supplement (
			task_id UUID NOT NULL, message_id UUID NOT NULL, chat_session_id UUID NOT NULL, workspace_id UUID NOT NULL,
			author_id UUID NOT NULL, status TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`INSERT INTO chat_task_supplement_fork_875 (message_id, task_id, workspace_id, chat_session_id, author_id, status, delivered_at)
		 VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'delivered', now()),
		        (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'failed', NULL)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := hook(ctx, pool); err != nil {
		t.Fatalf("hook: %v", err)
	}
	var copiedRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_task_supplement WHERE status = 'delivered'`).Scan(&copiedRows); err != nil || copiedRows != 1 {
		t.Fatalf("hook copied %d rows, want 1: %v", copiedRows, err)
	}
}

func TestSupplementForkCopyHookIsNoOpWithoutForkTable(t *testing.T) {
	admin := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, admin, "supplement_nofork_")
	pool := openTestPoolWithSearchPath(t, schema)
	if err := preMigrationHooks[supplementHookTarget(t)](ctx, pool); err != nil {
		t.Fatalf("hook without a fork table: %v", err)
	}
}
