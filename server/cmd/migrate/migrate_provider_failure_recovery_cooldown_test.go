package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CHE-10039: the 99zz48 down migration must stay runnable after the guard has
// stored a provider_cooldown_elapsed_unprobed row. It keeps the row (the
// failed_task_id key is what bounds recovery to one run) and re-labels it with
// the closest pre-99zz48 state, instead of refusing the rollback.
func TestProviderFailureRecoveryCooldownElapsedDownIsRollbackSafe(test *testing.T) {
	base := openTestPool(test)
	ctx, cancel := context.WithTimeout(test.Context(), time.Minute)
	defer cancel()
	schema := createScratchSchema(test, ctx, base, "pfr_cooldown_")
	pool := openTestPoolWithSearchPath(test, schema)
	run := func(version, direction string) error {
		test.Helper()
		sql, err := os.ReadFile(filepath.Join("..", "..", "migrations", version+"."+direction+".sql"))
		if err != nil {
			test.Fatal(err)
		}
		_, err = pool.Exec(ctx, string(sql))
		return err
	}
	for _, version := range []string{"592_provider_failure_recovery", "818_provider_failure_recovery_effects", "9998_provider_failure_recovery_success_evidence", "99zz48_provider_failure_recovery_cooldown_elapsed"} {
		if err := run(version, "up"); err != nil {
			test.Fatalf("up %s: %v", version, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider_failure_recovery
		(failed_task_id, trigger_kind, trigger_id, failed_at, probe_at, changed_condition, recovery_run_id)
		VALUES ('00000000-0000-0000-0000-000000000001', 'autopilot_schedule', '00000000-0000-0000-0000-0000000000aa', now() - interval '7 hours', now(), 'provider_cooldown_elapsed_unprobed', '00000000-0000-0000-0000-0000000000bb')`); err != nil {
		test.Fatalf("insert cooldown-elapsed row: %v", err)
	}

	if err := run("99zz48_provider_failure_recovery_cooldown_elapsed", "down"); err != nil {
		test.Fatalf("down refused with a cooldown-elapsed row present: %v", err)
	}
	var condition string
	if err := pool.QueryRow(ctx, `SELECT changed_condition FROM provider_failure_recovery WHERE failed_task_id = '00000000-0000-0000-0000-000000000001'`).Scan(&condition); err != nil {
		test.Fatalf("row lost by down (recovery would no longer be bounded): %v", err)
	}
	if condition != "provider_success_after_failure" {
		test.Fatalf("changed_condition after down = %q, want provider_success_after_failure", condition)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provider_failure_recovery
		(failed_task_id, trigger_kind, trigger_id, failed_at, probe_at, changed_condition, recovery_run_id)
		VALUES ('00000000-0000-0000-0000-000000000002', 'autopilot_schedule', '00000000-0000-0000-0000-0000000000aa', now(), now(), 'provider_cooldown_elapsed_unprobed', '00000000-0000-0000-0000-0000000000cc')`); err == nil {
		test.Fatal("old constraint still accepts provider_cooldown_elapsed_unprobed after down")
	}
}
