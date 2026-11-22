package main

import (
	"context"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// browserPermissionVersions lists the migrations CHE-4737 (W3-8) owns, in up order. They run
// after the W3-2 and W3-18 files and leave those as they are.
var browserPermissionVersions = []string{
	"99964c_browser_permission",
	"99964d_browser_profile_member_idx",
	"99964e_browser_profile_agent_idx",
}

func TestBrowserPermissionMigrationsFollowRepoRules(t *testing.T) {
	t.Parallel()
	assertBrowserMigrationsFollowRepoRules(t, browserPermissionVersions)
}

// CHE-4737: default deny means a grant is a row; a profile holds each member and each agent at
// most once; the migrations reverse cleanly.
func TestBrowserPermissionMigrationsStoreOneGrantPerPair(t *testing.T) {
	base := openTestPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	schema := createScratchSchema(t, ctx, base, "browser_permission_")
	pool := openTestPoolWithSearchPath(t, schema)
	all := slices.Concat(browserLeaseVersions, browserInstanceRegistryVersions, browserPermissionVersions)
	run := func(direction string) {
		t.Helper()
		versions := slices.Clone(all)
		if direction == "down" {
			slices.Reverse(versions)
		}
		if err := runMigrations(ctx, pool, runOptions{
			Direction:             direction,
			Files:                 realMigrationFiles(t, versions, direction),
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
		ws      = "00000000-0000-0000-0000-0000000000a1"
		profile = "00000000-0000-0000-0000-0000000000b1"
		user    = "00000000-0000-0000-0000-0000000000d1"
		agent   = "00000000-0000-0000-0000-0000000000e1"
	)
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if err := execBrowserLease(t, ctx, pool, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustExec(`INSERT INTO browser_profile (id, workspace_id, name, class) VALUES ($1, $2, 'figma', 'S2')`, profile, ws)
	mustExec(`INSERT INTO browser_profile_member (workspace_id, profile_id, user_id) VALUES ($1, $2, $3)`, ws, profile, user)
	wantPgCode(t, execBrowserLease(t, ctx, pool,
		`INSERT INTO browser_profile_member (workspace_id, profile_id, user_id) VALUES ($1, $2, $3)`, ws, profile, user),
		"23505", "second grant of the same member")
	mustExec(`INSERT INTO browser_profile_agent (workspace_id, profile_id, agent_id) VALUES ($1, $2, $3)`, ws, profile, agent)
	wantPgCode(t, execBrowserLease(t, ctx, pool,
		`INSERT INTO browser_profile_agent (workspace_id, profile_id, agent_id) VALUES ($1, $2, $3)`, ws, profile, agent),
		"23505", "second grant of the same agent")

	run("down")
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name LIKE 'browser_profile_%'`, schema).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d permission tables left after down", n)
	}
}
