CREATE TABLE provider_failure_recovery (
    failed_task_id UUID PRIMARY KEY,
    trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('autopilot_schedule', 'issue_wakeup')),
    trigger_id UUID NOT NULL,
    condition_key TEXT NOT NULL DEFAULT '',
    failed_at TIMESTAMPTZ NOT NULL,
    probe_at TIMESTAMPTZ NOT NULL,
    probe_status INTEGER NOT NULL CHECK (probe_status = 401),
    changed_condition TEXT NOT NULL CHECK (changed_condition = 'provider_server_error_to_http_401'),
    recovery_run_id UUID,
    recovery_task_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (num_nonnulls(recovery_run_id, recovery_task_id) = 1)
);

CREATE UNIQUE INDEX provider_failure_recovery_run_idx
    ON provider_failure_recovery (recovery_run_id)
    WHERE recovery_run_id IS NOT NULL;

CREATE UNIQUE INDEX provider_failure_recovery_task_idx
    ON provider_failure_recovery (recovery_task_id)
    WHERE recovery_task_id IS NOT NULL;
