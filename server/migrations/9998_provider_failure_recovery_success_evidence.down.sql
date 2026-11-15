DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM provider_failure_recovery WHERE changed_condition = 'provider_success_after_failure') THEN
        RAISE EXCEPTION 'cannot downgrade while success-evidence recoveries exist';
    END IF;
END $$;

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
