-- name: FindLatestScheduledAutopilotTaskForTrigger :one
SELECT
    task.id,
    task.status,
    task.failure_reason,
    COALESCE(task.completed_at, task.created_at) AS terminal_at,
    COALESCE(run.trigger_payload->>'head_sha', '') AS condition_key,
    EXISTS (
        SELECT 1 FROM provider_failure_recovery recovery
        WHERE recovery.recovery_task_id = task.id OR recovery.recovery_run_id = run.id
    ) AS is_recovery
FROM agent_task_queue task
JOIN autopilot_run run ON run.id = task.autopilot_run_id
    OR (task.autopilot_run_id IS NULL AND run.issue_id = task.issue_id)
WHERE run.trigger_id = @trigger_id
  AND run.source = 'schedule'
  AND task.status IN ('completed', 'failed', 'cancelled')
  AND COALESCE(run.trigger_payload->>'head_sha', '') = @condition_key::text
ORDER BY COALESCE(task.completed_at, task.created_at) DESC, task.created_at DESC
LIMIT 1;

-- name: FindActiveScheduledAutopilotRunForTrigger :one
SELECT * FROM autopilot_run
WHERE trigger_id = @trigger_id
  AND source = 'schedule'
  AND status IN ('pending', 'issue_created', 'running')
  AND planned_at = @planned_at::timestamptz
  AND COALESCE(trigger_payload->>'head_sha', '') = @condition_key::text
ORDER BY created_at DESC
LIMIT 1;

-- name: FindLatestRecurringWakeupTask :one
SELECT
    task.id,
    task.status,
    task.failure_reason,
    COALESCE(task.completed_at, task.created_at) AS terminal_at,
    COALESCE(task.context->>'head_sha', '') AS condition_key,
    EXISTS (
        SELECT 1 FROM provider_failure_recovery recovery
        WHERE recovery.recovery_task_id = task.id
    ) AS is_recovery
FROM agent_task_queue task
WHERE task.context->>'wakeup_id' = @wakeup_id::text
  AND task.status IN ('completed', 'failed', 'cancelled')
  AND COALESCE(task.context->>'head_sha', '') = @condition_key::text
ORDER BY COALESCE(task.completed_at, task.created_at) DESC, task.created_at DESC
LIMIT 1;

-- name: RecordProviderFailureRecovery :execrows
INSERT INTO provider_failure_recovery (
    failed_task_id, trigger_kind, trigger_id, condition_key, failed_at,
    probe_at, probe_status, changed_condition, recovery_run_id, recovery_task_id
)
VALUES (
    @failed_task_id, @trigger_kind, @trigger_id, @condition_key, @failed_at,
    @probe_at, @probe_status, @changed_condition, sqlc.narg(recovery_run_id), sqlc.narg(recovery_task_id)
)
ON CONFLICT (failed_task_id) DO NOTHING;
