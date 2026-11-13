-- CHE-4311: a scheduled trigger may recover from a provider 503 on same-provider
-- success evidence (breaker closed, last_success_at after the failure) instead
-- of an HTTP 401 probe. probe_at holds that last_success_at; probe_status stays NULL.
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
