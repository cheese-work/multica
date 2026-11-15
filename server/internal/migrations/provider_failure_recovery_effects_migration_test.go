package migrations

import (
	"context"
	"testing"
)

// TestProviderFailureRecoveryEffectsMigrationKeepsExistingRows applies 818 over
// rows written under 592, the shape every deployed database already holds: fresh
// schema CI never exercises the ALTER over existing data.
func TestProviderFailureRecoveryEffectsMigrationKeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	s := newTriageMigrationSandbox(t, "provider_failure_recovery_effects_migration_test")
	applyMigrationFile(t, ctx, s.pool, "592_provider_failure_recovery.up.sql")
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO provider_failure_recovery
			(failed_task_id, trigger_kind, trigger_id, failed_at, probe_at, probe_status, changed_condition, recovery_run_id, recovery_task_id)
		VALUES
			(gen_random_uuid(), 'autopilot_schedule', gen_random_uuid(), now(), now(), 401, 'provider_server_error_to_http_401', gen_random_uuid(), NULL),
			(gen_random_uuid(), 'issue_wakeup', gen_random_uuid(), now(), now(), 401, 'provider_server_error_to_http_401', NULL, gen_random_uuid())
	`); err != nil {
		t.Fatalf("seed rows valid under 592: %v", err)
	}

	applyMigrationFile(t, ctx, s.pool, "818_provider_failure_recovery_effects.up.sql")

	var rows, invalid int
	if err := s.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE recovery_wakeup_id IS NOT NULL) FROM provider_failure_recovery`).Scan(&rows, &invalid); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || invalid != 0 {
		t.Fatalf("rows after 818 = %d (with wakeup effect %d), want the 2 pre-existing rows untouched", rows, invalid)
	}
	// The new constraint is validated over the old rows and still refuses a bad shape.
	var validated bool
	if err := s.pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'provider_failure_recovery_effect_shape_check'`).Scan(&validated); err != nil || !validated {
		t.Fatalf("effect shape check validated=%t err=%v, want a validated constraint", validated, err)
	}
	assertInsertCheckViolation(t, ctx, s.pool, `
		INSERT INTO provider_failure_recovery (failed_task_id, trigger_kind, trigger_id, failed_at, changed_condition, recovery_task_id, recovery_wakeup_id, recovery_wakeup_revision)
		VALUES (gen_random_uuid(), 'issue_wakeup', gen_random_uuid(), now(), 'recovery_reserved', gen_random_uuid(), gen_random_uuid(), 1)`)
}
