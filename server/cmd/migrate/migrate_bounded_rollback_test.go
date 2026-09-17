package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CHE-530 — bounded rollback ("migrate down --to <version>").
//
// The bare `migrate down` walks every applied migration back to 001 with no
// way to stop early (server/cmd/migrate/main.go's runMigrations has no
// concept of a target version). A deploy rollback needs to undo exactly the
// migrations a bad release introduced and land back on the previous known-
// good SHA's schema version — not the entire history. This file tests the
// new boundDownFiles helper and its integration with runMigrations against a
// real Postgres, proving the ledger ends up at exactly the requested target
// and never walks past it even when older down migrations exist.
//
// Like migrate_concurrent_test.go, this connects to whatever DATABASE_URL
// points at (default postgres://multica:multica@localhost:5432/multica) and
// skips cleanly if unreachable. Each test uses a private schema and a unique
// advisory-lock key so it never touches the production schema_migrations
// table or blocks behind a real migration runner.

// boundedFixture is a per-test sandbox with both .up.sql and .down.sql files
// for a small ordered chain of migrations, each creating (up) / dropping
// (down) one marker table so applied/rolled-back state is externally
// observable beyond just the schema_migrations ledger.
type boundedFixture struct {
	pool     *pgxpool.Pool
	schema   string
	tableFQN string
	lockKey  int64
	upFiles  []string // sorted ascending, matches migrations.Files("up")
	versions []string // ascending, one per migration, e.g. "001_test_xyz"
	tables   []string // marker table name per migration, same order as versions
}

func newBoundedFixture(t *testing.T, numMigrations int) *boundedFixture {
	t.Helper()
	pool := openTestPool(t)

	suffix := fmt.Sprintf("%d_%d", time.Now().UnixNano(), rand.Uint32())
	schema := "migrate_bounded_test_" + suffix
	tableFQN := schema + ".schema_migrations"
	lockKey := int64(rand.Uint64()&0x7fffffffffffffff) | 1

	ctx := context.Background()
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, pgx.Identifier{schema}.Sanitize())); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, pgx.Identifier{schema}.Sanitize())); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})

	dir := t.TempDir()
	versions := make([]string, 0, numMigrations)
	tables := make([]string, 0, numMigrations)
	upFiles := make([]string, 0, numMigrations)
	for i := 0; i < numMigrations; i++ {
		version := fmt.Sprintf("%03d_test_%s", i+1, suffix)
		table := fmt.Sprintf("t_%s_%d", suffix, i+1)
		versions = append(versions, version)
		tables = append(tables, table)

		upBody := fmt.Sprintf("CREATE TABLE %s.%s (id BIGSERIAL PRIMARY KEY);\n",
			pgx.Identifier{schema}.Sanitize(), pgx.Identifier{table}.Sanitize())
		upPath := filepath.Join(dir, version+".up.sql")
		if err := os.WriteFile(upPath, []byte(upBody), 0o600); err != nil {
			t.Fatalf("write up migration: %v", err)
		}
		upFiles = append(upFiles, upPath)

		downBody := fmt.Sprintf("DROP TABLE %s.%s;\n",
			pgx.Identifier{schema}.Sanitize(), pgx.Identifier{table}.Sanitize())
		downPath := filepath.Join(dir, version+".down.sql")
		if err := os.WriteFile(downPath, []byte(downBody), 0o600); err != nil {
			t.Fatalf("write down migration: %v", err)
		}
	}
	sort.Strings(upFiles)

	return &boundedFixture{
		pool:     pool,
		schema:   schema,
		tableFQN: tableFQN,
		lockKey:  lockKey,
		upFiles:  upFiles,
		versions: versions,
		tables:   tables,
	}
}

// downFiles returns every .down.sql this fixture wrote, in the same
// reverse-lexicographic order migrations.Files("down") would produce —
// newest migration first — which is the order both runMigrations and
// boundDownFiles expect to walk.
func (f *boundedFixture) downFiles() []string {
	files := make([]string, len(f.upFiles))
	for i, up := range f.upFiles {
		files[i] = up[:len(up)-len(".up.sql")] + ".down.sql"
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	return files
}

func (f *boundedFixture) upOpts() runOptions {
	return runOptions{
		Direction:             "up",
		Files:                 f.upFiles,
		SchemaMigrationsTable: f.tableFQN,
		AdvisoryLockKey:       f.lockKey,
	}
}

func (f *boundedFixture) downOpts(files []string) runOptions {
	return runOptions{
		Direction:             "down",
		Files:                 files,
		SchemaMigrationsTable: f.tableFQN,
		AdvisoryLockKey:       f.lockKey,
	}
}

func (f *boundedFixture) appliedVersions(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := f.pool.Query(ctx,
		fmt.Sprintf(`SELECT version FROM %s ORDER BY version`,
			pgx.Identifier{f.schema, "schema_migrations"}.Sanitize()))
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

func (f *boundedFixture) tableExists(t *testing.T, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var exists bool
	if err := f.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = $2
		)`, f.schema, name).Scan(&exists); err != nil {
		t.Fatalf("check table %s.%s: %v", f.schema, name, err)
	}
	return exists
}

// TestBoundDownFilesStopsAtTarget is a pure unit test (no Postgres) that
// pins the file-slicing contract: rolling "down --to" a target keeps that
// target applied and removes only the strictly-newer migrations, in the
// same reverse order runMigrations already walks.
func TestBoundDownFilesStopsAtTarget(t *testing.T) {
	// Mirrors a reverse-sorted "down" file list the way migrations.Files
	// would return it for versions 001..005.
	all := []string{
		"/m/005_five.down.sql",
		"/m/004_four.down.sql",
		"/m/003_three.down.sql",
		"/m/002_two.down.sql",
		"/m/001_one.down.sql",
	}

	got, err := boundDownFiles(all, "003_three")
	if err != nil {
		t.Fatalf("boundDownFiles: %v", err)
	}
	want := []string{"/m/005_five.down.sql", "/m/004_four.down.sql"}
	if len(got) != len(want) {
		t.Fatalf("boundDownFiles(..., %q) = %v, want %v", "003_three", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("boundDownFiles(..., %q)[%d] = %q, want %q", "003_three", i, got[i], want[i])
		}
	}
}

// TestBoundDownFilesTargetIsNewest confirms asking to stop at the newest
// version is a no-op (nothing rolls back), rather than an off-by-one error
// that drops it too.
func TestBoundDownFilesTargetIsNewest(t *testing.T) {
	all := []string{"/m/003_three.down.sql", "/m/002_two.down.sql", "/m/001_one.down.sql"}
	got, err := boundDownFiles(all, "003_three")
	if err != nil {
		t.Fatalf("boundDownFiles: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("boundDownFiles(..., newest) = %v, want empty", got)
	}
}

// TestBoundDownFilesTargetIsOldest confirms rolling back to the oldest known
// version reverses everything except that first migration — the same
// boundary the legacy unbounded "down" would reach if version 001 is the
// true floor, but reached deliberately via --to instead of by exhausting the
// file list.
func TestBoundDownFilesTargetIsOldest(t *testing.T) {
	all := []string{"/m/003_three.down.sql", "/m/002_two.down.sql", "/m/001_one.down.sql"}
	got, err := boundDownFiles(all, "001_one")
	if err != nil {
		t.Fatalf("boundDownFiles: %v", err)
	}
	want := []string{"/m/003_three.down.sql", "/m/002_two.down.sql"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("boundDownFiles(..., oldest) = %v, want %v", got, want)
	}
}

// TestBoundDownFilesUnknownVersion pins the fail-closed contract: a typo'd
// or nonexistent --to target must error, never silently fall through to the
// unbounded full-reverse walk. That silent fallback is exactly the overshoot
// this flag exists to prevent.
func TestBoundDownFilesUnknownVersion(t *testing.T) {
	all := []string{"/m/002_two.down.sql", "/m/001_one.down.sql"}
	_, err := boundDownFiles(all, "999_does_not_exist")
	if err == nil {
		t.Fatal("boundDownFiles with unknown version: want error, got nil")
	}
}

// TestParseToFlagRejectsWithUp pins that --to only makes sense paired with
// "down": bounding an "up" run by a stop-before version is not what this
// flag is for (up already only applies pending migrations in order), and
// silently accepting it would invite confusion about what it does.
func TestParseToFlagRejectsWithUp(t *testing.T) {
	if _, err := parseToFlag("up", []string{"--to", "010_x"}); err == nil {
		t.Fatal("parseToFlag(up, --to ...): want error, got nil")
	}
}

// TestParseToFlagAbsentIsUnbounded confirms that omitting --to entirely
// returns "" for both directions, which callers treat as "run the legacy
// unbounded behavior" — the required backward-compatibility guarantee.
func TestParseToFlagAbsentIsUnbounded(t *testing.T) {
	for _, direction := range []string{"up", "down"} {
		got, err := parseToFlag(direction, nil)
		if err != nil {
			t.Fatalf("parseToFlag(%s, nil): unexpected error: %v", direction, err)
		}
		if got != "" {
			t.Fatalf("parseToFlag(%s, nil) = %q, want \"\"", direction, got)
		}
	}
}

// TestParseToFlagAcceptsDown confirms the happy path parses cleanly and
// rejects unexpected positional arguments trailing the flag.
func TestParseToFlagAcceptsDown(t *testing.T) {
	got, err := parseToFlag("down", []string{"--to", "010_x"})
	if err != nil {
		t.Fatalf("parseToFlag(down, --to 010_x): unexpected error: %v", err)
	}
	if got != "010_x" {
		t.Fatalf("parseToFlag(down, --to 010_x) = %q, want %q", got, "010_x")
	}

	if _, err := parseToFlag("down", []string{"--to", "010_x", "extra"}); err == nil {
		t.Fatal("parseToFlag with trailing positional arg: want error, got nil")
	}
}

// TestRunMigrationsBoundedRollbackAgainstRealPostgres is the live-Postgres
// proof the issue asks for: forward N migrations to HEAD, roll back M of
// them via boundDownFiles + runMigrations(down), and confirm:
//  1. schema_migrations ends at exactly the target version (every strictly
//     newer version is gone, the target and everything older remains).
//  2. The marker tables for rolled-back migrations are actually dropped
//     (schema state matches the ledger, not just the ledger by itself).
//  3. The marker tables at and below the target version still exist
//     (the bound did not walk past its stop point).
//  4. Running the exact same bounded down again is a safe no-op (every
//     migration above the target is already absent from the ledger, so
//     runMigrations's existing "not applied" skip path handles it — the
//     bound doesn't need its own idempotency, it inherits the loop's).
func TestRunMigrationsBoundedRollbackAgainstRealPostgres(t *testing.T) {
	const numMigrations = 8
	const rollbackToIndex = 4 // 0-based: keep versions[0..4], roll back versions[5..7]
	f := newBoundedFixture(t, numMigrations)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Forward to HEAD.
	if err := runMigrations(ctx, f.pool, f.upOpts()); err != nil {
		t.Fatalf("runMigrations(up) to HEAD: %v", err)
	}
	if got := f.appliedVersions(t); len(got) != numMigrations {
		t.Fatalf("after up: schema_migrations has %d rows, want %d (%v)", len(got), numMigrations, got)
	}
	for _, tbl := range f.tables {
		if !f.tableExists(t, tbl) {
			t.Fatalf("after up: table %s.%s missing", f.schema, tbl)
		}
	}

	targetVersion := f.versions[rollbackToIndex]
	bounded, err := boundDownFiles(f.downFiles(), targetVersion)
	if err != nil {
		t.Fatalf("boundDownFiles(..., %q): %v", targetVersion, err)
	}
	wantRolledBackCount := numMigrations - rollbackToIndex - 1
	if len(bounded) != wantRolledBackCount {
		t.Fatalf("boundDownFiles(..., %q) selected %d files, want %d", targetVersion, len(bounded), wantRolledBackCount)
	}

	if err := runMigrations(ctx, f.pool, f.downOpts(bounded)); err != nil {
		t.Fatalf("runMigrations(down, bounded to %q): %v", targetVersion, err)
	}

	// 1. Ledger ends at exactly the target: versions[0..rollbackToIndex]
	// remain, versions[rollbackToIndex+1..] are gone. Never further.
	wantRemaining := append([]string(nil), f.versions[:rollbackToIndex+1]...)
	sort.Strings(wantRemaining)
	if got := f.appliedVersions(t); !equalStrings(got, wantRemaining) {
		t.Fatalf("schema_migrations after bounded rollback = %v, want %v", got, wantRemaining)
	}

	// 2. Marker tables for rolled-back migrations are actually dropped —
	// schema state, not just ledger rows, reflects the target version.
	for i := rollbackToIndex + 1; i < numMigrations; i++ {
		if f.tableExists(t, f.tables[i]) {
			t.Fatalf("table %s.%s for rolled-back version %s still exists", f.schema, f.tables[i], f.versions[i])
		}
	}

	// 3. Everything at or below the target survives untouched — the bound
	// did not walk past its stop point even though older down migrations
	// exist and remain on disk.
	for i := 0; i <= rollbackToIndex; i++ {
		if !f.tableExists(t, f.tables[i]) {
			t.Fatalf("table %s.%s for kept version %s was dropped; bounded rollback walked past --to %q",
				f.schema, f.tables[i], f.versions[i], targetVersion)
		}
	}

	// 4. Re-running the identical bounded rollback is a no-op: every
	// migration above the target is already absent, so each hits
	// runMigrations's "not applied" skip branch. No error, no double-drop.
	if err := runMigrations(ctx, f.pool, f.downOpts(bounded)); err != nil {
		t.Fatalf("second runMigrations(down, bounded to %q) should be a no-op, got error: %v", targetVersion, err)
	}
	if got := f.appliedVersions(t); !equalStrings(got, wantRemaining) {
		t.Fatalf("schema_migrations after repeated bounded rollback = %v, want %v (unchanged)", got, wantRemaining)
	}
}

// TestRunMigrationsBoundedRollbackStopsEvenWithMoreDownFilesOnDisk proves
// the "stop even if further down-migrations exist" requirement directly:
// the fixture always has down files available all the way to version 001,
// but bounding to an interior version must never touch anything older than
// that target, regardless of how much more history is on disk.
func TestRunMigrationsBoundedRollbackStopsEvenWithMoreDownFilesOnDisk(t *testing.T) {
	const numMigrations = 10
	const rollbackToIndex = 7 // roll back only the last two (indices 8, 9)
	f := newBoundedFixture(t, numMigrations)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := runMigrations(ctx, f.pool, f.upOpts()); err != nil {
		t.Fatalf("runMigrations(up) to HEAD: %v", err)
	}

	targetVersion := f.versions[rollbackToIndex]
	// Sanity: there are far more down migrations available on disk than the
	// bound should ever touch.
	allDown := f.downFiles()
	if len(allDown) != numMigrations {
		t.Fatalf("fixture wiring: len(downFiles()) = %d, want %d", len(allDown), numMigrations)
	}

	bounded, err := boundDownFiles(allDown, targetVersion)
	if err != nil {
		t.Fatalf("boundDownFiles(..., %q): %v", targetVersion, err)
	}
	if len(bounded) != numMigrations-rollbackToIndex-1 {
		t.Fatalf("boundDownFiles selected %d files, want %d", len(bounded), numMigrations-rollbackToIndex-1)
	}

	if err := runMigrations(ctx, f.pool, f.downOpts(bounded)); err != nil {
		t.Fatalf("runMigrations(down, bounded): %v", err)
	}

	// Every migration at or below the target — including version 001, the
	// oldest — must still be applied and its table intact, proving the
	// runner stopped at the requested version instead of continuing toward
	// 001 the way a bare unbounded `down` would.
	for i := 0; i <= rollbackToIndex; i++ {
		if !f.tableExists(t, f.tables[i]) {
			t.Fatalf("table for version %s (index %d, at/below target) was dropped; bounded rollback overshot --to %q",
				f.versions[i], i, targetVersion)
		}
	}
	for i := rollbackToIndex + 1; i < numMigrations; i++ {
		if f.tableExists(t, f.tables[i]) {
			t.Fatalf("table for version %s (index %d, above target) still exists after bounded rollback", f.versions[i], i)
		}
	}
}
