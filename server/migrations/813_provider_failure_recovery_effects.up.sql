ALTER TABLE provider_failure_recovery
    ADD COLUMN recovery_wakeup_id UUID,
    ADD COLUMN recovery_wakeup_revision BIGINT,
    ALTER COLUMN probe_at DROP NOT NULL,
    ALTER COLUMN probe_status DROP NOT NULL,
    DROP CONSTRAINT IF EXISTS provider_failure_recovery_check,
    DROP CONSTRAINT IF EXISTS provider_failure_recovery_probe_status_check,
    DROP CONSTRAINT IF EXISTS provider_failure_recovery_changed_condition_check,
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

CREATE UNIQUE INDEX provider_failure_recovery_wakeup_idx
    ON provider_failure_recovery (recovery_wakeup_id)
    WHERE recovery_wakeup_id IS NOT NULL;
