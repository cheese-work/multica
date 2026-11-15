package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// browserLeaseVersions lists the migrations CHE-4731 (W3-2) owns, in up order.
var browserLeaseVersions = []string{
	"99960_browser_lease",
	"99961_browser_profile_name_idx",
	"99962_browser_instance_name_idx",
	"99963_browser_lease_one_holder_idx",
	"99964_browser_lease_epoch_idx",
}

func browserLeaseVersionsFor(direction string) []string {
	v := slices.Clone(browserLeaseVersions)
	if direction == "down" {
		slices.Reverse(v)
	}
	return v
}

// Repo AGENTS.md "Database and Migration Rules": no foreign keys or cascades; every index is
// CREATE [UNIQUE] INDEX CONCURRENTLY in its own single-statement file; PRIMARY KEY is the only
// inline index.
func TestBrowserLeaseMigrationsFollowRepoRules(t *testing.T) {
	t.Parallel()
	comment := regexp.MustCompile(`(?m)--.*$`)
	for _, version := range browserLeaseVersions {
		for _, direction := range []string{"up", "down"} {
			f := filepath.Join("..", "..", "migrations", version+"."+direction+".sql")
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			sql := strings.ToUpper(comment.ReplaceAllString(string(raw), ""))
			if strings.Contains(sql, "REFERENCES") || strings.Contains(sql, "CASCADE") {
				t.Errorf("%s: foreign key or cascade", f)
			}
			if strings.Contains(sql, "UNIQUE (") || strings.Contains(sql, "UNIQUE(") {
				t.Errorf("%s: inline unique constraint builds a non-concurrent index", f)
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
}

func execBrowserLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) error {
	t.Helper()
	_, err := pool.Exec(ctx, sql, args...)
	return err
}

func wantPgCode(t *testing.T, err error, code, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: want SQLSTATE %s, got %v", what, code, err)
	}
}

// CHE-4731: browser_profile, browser_lease and browser_instance (epoch, phase). One lease holds an
// instance at a time, the database refuses a second one, and the migrations reverse cleanly.
func TestBrowserLeaseMigrationsConstrainInstanceHolder(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "browser_lease_")
	pool := openTestPoolWithSearchPath(t, schema)
	run := func(direction string) {
		t.Helper()
		if err := runMigrations(ctx, pool, runOptions{
			Direction:             direction,
			Files:                 realMigrationFiles(t, browserLeaseVersionsFor(direction), direction),
			SchemaMigrationsTable: schema + ".schema_migrations",
			AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
			Hooks:                 hooksForDirection(direction),
			Conditions:            conditionsForDirection(direction),
		}); err != nil {
			t.Fatalf("migrate %s: %v", direction, err)
		}
	}
	run("up")

	const (
		ws       = "00000000-0000-0000-0000-0000000000a1"
		profile  = "00000000-0000-0000-0000-0000000000b1"
		instance = "00000000-0000-0000-0000-0000000000c1"
		other    = "00000000-0000-0000-0000-0000000000c2"
	)
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if err := execBrowserLease(t, ctx, pool, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustExec(`INSERT INTO browser_profile (id, workspace_id, name, class) VALUES ($1, $2, 'figma', 'S2')`, profile, ws)
	mustExec(`INSERT INTO browser_instance (id, workspace_id, name) VALUES ($1, $2, 'steel-1'), ($3, $2, 'steel-2')`, instance, ws, other)

	var epoch int64
	var phase string
	if err := pool.QueryRow(ctx, `SELECT epoch, phase FROM browser_instance WHERE id = $1`, instance).Scan(&epoch, &phase); err != nil {
		t.Fatal(err)
	}
	if epoch != 0 || phase != "idle_clean" {
		t.Errorf("new instance = epoch %d phase %s, want 0 idle_clean", epoch, phase)
	}

	insertLease := func(instanceID, state string, leaseEpoch *int64) error {
		return execBrowserLease(t, ctx, pool, `
			INSERT INTO browser_lease (workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, class, state, epoch, expires_at)
			VALUES ($1, $2, $3, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'S2', $4, $5, now() + interval '2 hours')`,
			ws, instanceID, profile, state, leaseEpoch)
	}
	ep := func(n int64) *int64 { return &n }

	if err := insertLease(instance, "active", ep(1)); err != nil {
		t.Fatalf("first active lease: %v", err)
	}
	wantPgCode(t, insertLease(instance, "active", ep(2)), "23505", "second active lease on one instance")
	if err := insertLease(other, "active", ep(1)); err != nil {
		t.Errorf("active lease on another instance: %v", err)
	}
	// Queued leases are not holders: any number may wait.
	for range 3 {
		if err := insertLease(instance, "queued", nil); err != nil {
			t.Errorf("queued lease: %v", err)
		}
	}
	// A reused epoch on one instance is a stale fence.
	wantPgCode(t, execBrowserLease(t, ctx, pool, `
		INSERT INTO browser_lease (workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, class, state, epoch, expires_at, ended_at, end_reason, cleanup_acked_at)
		VALUES ($1, $2, $3, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 'S2', 'ended', 1, now(), now(), 'release', now())`,
		ws, instance, profile), "23505", "reused epoch")

	// An ended lease keeps the instance until cleanup-ack; only then may the next one hold it.
	mustExec(`UPDATE browser_lease SET state = 'ended', ended_at = now(), end_reason = 'release' WHERE instance_id = $1 AND state = 'active'`, instance)
	wantPgCode(t, insertLease(instance, "active", ep(2)), "23505", "grant before cleanup-ack")
	mustExec(`UPDATE browser_lease SET cleanup_acked_at = now() WHERE instance_id = $1 AND state = 'ended'`, instance)
	if err := insertLease(instance, "active", ep(2)); err != nil {
		t.Errorf("grant after cleanup-ack: %v", err)
	}

	// Rejected shapes.
	wantPgCode(t, execBrowserLease(t, ctx, pool, `INSERT INTO browser_profile (workspace_id, name, class) VALUES ($1, 'ro', 'S1')`, ws), "23514", "class S1")
	wantPgCode(t, execBrowserLease(t, ctx, pool, `INSERT INTO browser_profile (workspace_id, name, class) VALUES ($1, 'figma', 'S0')`, ws), "23505", "duplicate profile name")
	wantPgCode(t, execBrowserLease(t, ctx, pool, `INSERT INTO browser_instance (workspace_id, name, phase) VALUES ($1, 'x', 'bogus')`, ws), "23514", "unknown phase")
	wantPgCode(t, execBrowserLease(t, ctx, pool, `INSERT INTO browser_instance (workspace_id, name) VALUES ($1, 'steel-1')`, ws), "23505", "duplicate instance name")
	wantPgCode(t, insertLease(other, "active", nil), "23514", "active lease without epoch")
	wantPgCode(t, insertLease(other, "queued", ep(9)), "23514", "queued lease with epoch")
	wantPgCode(t, insertLease(other, "bogus", nil), "23514", "unknown lease state")

	run("down")
	for _, table := range []string{"browser_lease", "browser_instance", "browser_profile"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2)`, schema, table).Scan(&exists); err != nil || exists {
			t.Errorf("table %s after down: exists=%v err=%v", table, exists, err)
		}
	}
	run("up") // re-runnable
}
