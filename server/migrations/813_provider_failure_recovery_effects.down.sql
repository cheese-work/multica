DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM provider_failure_recovery
        WHERE changed_condition = 'recovery_reserved'
           OR recovery_wakeup_id IS NOT NULL
           OR recovery_wakeup_revision IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'cannot downgrade provider failure recovery effects while new reservations exist';
    END IF;
END $$;

DROP INDEX IF EXISTS provider_failure_recovery_wakeup_idx;

ALTER TABLE provider_failure_recovery
    DROP CONSTRAINT IF EXISTS provider_failure_recovery_effect_shape_check,
    ALTER COLUMN probe_at SET NOT NULL,
    ALTER COLUMN probe_status SET NOT NULL,
    ADD CONSTRAINT provider_failure_recovery_check CHECK (num_nonnulls(recovery_run_id, recovery_task_id) = 1),
    ADD CONSTRAINT provider_failure_recovery_probe_status_check CHECK (probe_status = 401),
    ADD CONSTRAINT provider_failure_recovery_changed_condition_check CHECK (changed_condition = 'provider_server_error_to_http_401'),
    DROP COLUMN recovery_wakeup_revision,
    DROP COLUMN recovery_wakeup_id;
