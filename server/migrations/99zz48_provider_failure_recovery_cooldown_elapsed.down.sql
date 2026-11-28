-- Rollback-safe (CHE-10039): keep cooldown-elapsed rows instead of refusing.
-- The failed_task_id key is what bounds recovery to one run, so deleting a row
-- would let that task recover again. provider_success_after_failure has the
-- same column shape (probe_at set, probe_status NULL, one run or task), so the
-- older constraint accepts the re-labelled row.
UPDATE provider_failure_recovery
SET changed_condition = 'provider_success_after_failure'
WHERE changed_condition = 'provider_cooldown_elapsed_unprobed';

ALTER TABLE provider_failure_recovery
    DROP CONSTRAINT provider_failure_recovery_effect_shape_check,
    ADD CONSTRAINT provider_failure_recovery_effect_shape_check CHECK (
        (
            changed_condition = 'provider_server_error_to_http_401'
            AND probe_at IS NOT NULL
            AND probe_status = 401
            AND recovery_wakeup_id IS NULL
            AND recovery_wakeup_revision IS NULL
            AND num_nonnulls(recovery_run_id, recovery_task_id) = 1
        )
        OR
        (
            changed_condition = 'provider_success_after_failure'
            AND probe_at IS NOT NULL
            AND probe_status IS NULL
            AND recovery_wakeup_id IS NULL
            AND recovery_wakeup_revision IS NULL
            AND num_nonnulls(recovery_run_id, recovery_task_id) = 1
        )
        OR
        (
            changed_condition = 'recovery_reserved'
            AND probe_at IS NULL
            AND probe_status IS NULL
            AND (
                (
                    recovery_task_id IS NOT NULL
                    AND recovery_run_id IS NULL
                    AND recovery_wakeup_id IS NULL
                    AND recovery_wakeup_revision IS NULL
                )
                OR
                (
                    recovery_wakeup_id IS NOT NULL
                    AND recovery_wakeup_revision IS NOT NULL
                    AND recovery_run_id IS NULL
                    AND recovery_task_id IS NULL
                )
            )
        )
    );
