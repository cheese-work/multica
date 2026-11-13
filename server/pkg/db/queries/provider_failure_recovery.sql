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
JOIN autopilot ap ON ap.id = run.autopilot_id
WHERE run.trigger_id = @trigger_id
  AND run.source = 'schedule'
  AND task.status IN ('completed', 'failed', 'cancelled')
  AND COALESCE(run.trigger_payload->>'head_sha', '') = @condition_key::text
  AND (
      task.autopilot_run_id IS NOT NULL
      OR (
          task.trigger_comment_id IS NULL
          AND task.delegated_from_task_id IS NULL
          AND task.rerun_of_task_id IS NULL
          AND COALESCE(task.originator_source, '') <> 'direct_human'
          AND COALESCE(task.context->>'wakeup_id', '') = ''
          AND (
              (ap.assignee_type = 'agent' AND task.agent_id = ap.assignee_id AND task.squad_id IS NULL)
              OR (ap.assignee_type = 'squad' AND task.squad_id = COALESCE(run.squad_id, ap.assignee_id) AND task.is_leader_task = TRUE)
          )
          AND (task.status = 'completed' OR run.status <> 'completed')
          AND NOT EXISTS (
              SELECT 1 FROM agent_task_queue prior
              WHERE prior.issue_id = run.issue_id
                AND prior.id <> task.id
                AND prior.autopilot_run_id IS NULL
                AND prior.trigger_comment_id IS NULL
                AND prior.delegated_from_task_id IS NULL
                AND prior.rerun_of_task_id IS NULL
                AND COALESCE(prior.originator_source, '') <> 'direct_human'
                AND COALESCE(prior.context->>'wakeup_id', '') = ''
                AND (
                    (ap.assignee_type = 'agent' AND prior.agent_id = ap.assignee_id AND prior.squad_id IS NULL)
                    OR (ap.assignee_type = 'squad' AND prior.squad_id = COALESCE(run.squad_id, ap.assignee_id) AND prior.is_leader_task = TRUE)
                )
                AND prior.status = 'completed'
                AND (
                    task.status <> 'completed'
                    OR (prior.created_at, prior.id) < (task.created_at, task.id)
                )
          )
      )
  )
ORDER BY COALESCE(task.completed_at, task.created_at) DESC, task.created_at DESC
LIMIT 1;

-- name: FindActiveScheduledAutopilotRunForTrigger :one
SELECT * FROM autopilot_run
WHERE trigger_id = @trigger_id
  AND source = 'schedule'
  AND status IN ('pending', 'issue_created', 'running')
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
    probe_at, probe_status, changed_condition, recovery_run_id, recovery_task_id,
    recovery_wakeup_id, recovery_wakeup_revision
)
VALUES (
    @failed_task_id, @trigger_kind, @trigger_id, @condition_key, @failed_at,
    sqlc.narg('probe_at')::timestamptz, sqlc.narg('probe_status')::integer,
    @changed_condition, sqlc.narg(recovery_run_id), sqlc.narg(recovery_task_id),
    sqlc.narg('recovery_wakeup_id')::uuid, sqlc.narg('recovery_wakeup_revision')::bigint
)
ON CONFLICT DO NOTHING;

-- name: CreateProviderFailureOwnerNotice :execrows
INSERT INTO inbox_item (
    id, workspace_id, recipient_type, recipient_id, type, severity,
    issue_id, title, body, actor_type, details
)
VALUES (
    @id, @workspace_id, 'member', @recipient_id,
    'provider_failure_recovery_blocked', 'action_required',
    sqlc.narg('issue_id')::uuid,
    'Provider recovery needs attention',
    'An automatic provider recovery also failed. This recurring trigger will stay suppressed until its condition changes. Review the provider and trigger, then update or disable the trigger; no further recovery dispatch will occur automatically.',
    'system', @details
)
ON CONFLICT (id) DO NOTHING;

-- Issue-run wording for the cap notice: the failed run belongs to an issue, not
-- to a recurring trigger. Shares the failed task id as inbox id with the guard
-- notice, so one failed task can never tell its user twice.
-- name: CreateProviderFailureCapNotice :execrows
INSERT INTO inbox_item (
    id, workspace_id, recipient_type, recipient_id, type, severity,
    issue_id, title, body, actor_type, details
)
VALUES (
    @id, @workspace_id, 'member', @recipient_id,
    'provider_failure_recovery_blocked', 'action_required',
    sqlc.narg('issue_id')::uuid,
    'Provider recovery needs attention',
    'A provider failure already had an automatic recovery run, so this run will not be retried again. Check the provider, then rerun the issue by hand when it is healthy.',
    'system', @details
)
ON CONFLICT (id) DO NOTHING;

-- name: GetProviderFailureTriggerID :one
SELECT COALESCE(NULLIF(context->>'wakeup_id', '')::uuid, id)::uuid AS trigger_id
FROM agent_task_queue
WHERE id = @task_id;

-- name: GetProviderFailureNoticeTarget :one
SELECT
    COALESCE(issue.workspace_id, agent.workspace_id) AS workspace_id,
    task.accountable_user_id,
    task.issue_id
FROM agent_task_queue task
LEFT JOIN issue ON issue.id = task.issue_id
LEFT JOIN agent ON agent.id = task.agent_id
WHERE task.id = @task_id;

-- Inputs of the canonical recovery owner. The guard-owned predicates repeat the
-- conditions of FindLatestRecurringWakeupTask and
-- FindLatestScheduledAutopilotTaskForTrigger without their "latest task" ordering.
-- name: GetProviderFailureOwnerFacts :one
WITH RECURSIVE lineage(id) AS (
    SELECT t.id FROM agent_task_queue t WHERE t.id = @task_id
    UNION
    SELECT parent.id
    FROM lineage
    JOIN agent_task_queue child ON child.id = lineage.id
    JOIN agent_task_queue parent ON parent.id IN (child.retry_of_task_id, child.parent_task_id)
)
SELECT
    EXISTS (
        SELECT 1 FROM provider_failure_recovery recovery
        WHERE recovery.recovery_task_id IN (SELECT id FROM lineage)
    ) AS task_cap,
    EXISTS (
        SELECT 1 FROM provider_failure_recovery recovery
        WHERE recovery.recovery_wakeup_id = wakeup.id
    ) AS wakeup_cap,
    (COALESCE(task.context->>'wakeup_id', '') <> '')::boolean AS has_wakeup,
    COALESCE(wakeup.mode = 'once' AND wakeup.system_rule IS NULL, FALSE)::boolean AS wakeup_once,
    COALESCE(wakeup.mode = 'continuous' AND wakeup.kind IN ('every', 'cron') AND wakeup.system_rule IS NULL, FALSE)::boolean AS wakeup_guard_owned,
    EXISTS (
        SELECT 1
        FROM autopilot_run run
        JOIN autopilot ap ON ap.id = run.autopilot_id
        WHERE run.source = 'schedule'
          AND (
              run.id = task.autopilot_run_id
              OR (
                  task.autopilot_run_id IS NULL
                  AND run.issue_id = task.issue_id
                  AND task.trigger_comment_id IS NULL
                  AND task.delegated_from_task_id IS NULL
                  AND task.rerun_of_task_id IS NULL
                  AND COALESCE(task.originator_source, '') <> 'direct_human'
                  AND COALESCE(task.context->>'wakeup_id', '') = ''
                  AND (
                      (ap.assignee_type = 'agent' AND task.agent_id = ap.assignee_id AND task.squad_id IS NULL)
                      OR (ap.assignee_type = 'squad' AND task.squad_id = COALESCE(run.squad_id, ap.assignee_id) AND task.is_leader_task = TRUE)
                  )
                  AND (task.status = 'completed' OR run.status <> 'completed')
                  AND NOT EXISTS (
                      SELECT 1 FROM agent_task_queue prior
                      WHERE prior.issue_id = run.issue_id
                        AND prior.id <> task.id
                        AND prior.autopilot_run_id IS NULL
                        AND prior.trigger_comment_id IS NULL
                        AND prior.delegated_from_task_id IS NULL
                        AND prior.rerun_of_task_id IS NULL
                        AND COALESCE(prior.originator_source, '') <> 'direct_human'
                        AND COALESCE(prior.context->>'wakeup_id', '') = ''
                        AND (
                            (ap.assignee_type = 'agent' AND prior.agent_id = ap.assignee_id AND prior.squad_id IS NULL)
                            OR (ap.assignee_type = 'squad' AND prior.squad_id = COALESCE(run.squad_id, ap.assignee_id) AND prior.is_leader_task = TRUE)
                        )
                        AND prior.status = 'completed'
                        AND (
                            task.status <> 'completed'
                            OR (prior.created_at, prior.id) < (task.created_at, task.id)
                        )
                  )
              )
          )
    ) AS autopilot_guard_owned,
    (task.issue_id IS NOT NULL)::boolean AS has_issue,
    COALESCE(issue.triage_state IS NOT NULL, FALSE)::boolean AS in_triage,
    EXISTS (
        SELECT 1 FROM autopilot_run run
        WHERE run.id = task.autopilot_run_id AND run.source <> 'schedule'
    ) AS non_schedule_autopilot
FROM agent_task_queue task
LEFT JOIN issue_wakeup wakeup ON wakeup.id::text = task.context->>'wakeup_id'
LEFT JOIN issue ON issue.id = task.issue_id
WHERE task.id = @task_id;

-- CHE-1773: is this task in a provider-failure recovery lineage? Used to fence
-- every generic retry entry point. The ancestor walk is capped at 32 hops and
-- never revisits a task, so a cycle ends; a chain that still has a parent at the
-- cap is reported as too_deep and the caller treats it as a hit. The wakeup
-- match is revision-aware: a run at or after the recovery revision is covered,
-- an earlier run of the same wakeup is not.
-- name: GetRecoveryLineageFacts :one
WITH RECURSIVE lineage(id, depth, path) AS (
    SELECT t.id, 0, ARRAY[t.id] FROM agent_task_queue t WHERE t.id = @task_id
    UNION ALL
    SELECT parent.id, lineage.depth + 1, lineage.path || parent.id
    FROM lineage
    JOIN agent_task_queue child ON child.id = lineage.id
    JOIN agent_task_queue parent ON parent.id IN (child.retry_of_task_id, child.parent_task_id)
    WHERE lineage.depth < 32 AND NOT parent.id = ANY (lineage.path)
)
SELECT
    EXISTS (
        SELECT 1 FROM provider_failure_recovery recovery
        WHERE recovery.recovery_task_id IN (SELECT id FROM lineage)
    ) AS task_lineage,
    EXISTS (
        SELECT 1
        FROM lineage
        JOIN agent_task_queue child ON child.id = lineage.id
        WHERE lineage.depth = 32
          AND (child.retry_of_task_id IS NOT NULL OR child.parent_task_id IS NOT NULL)
    ) AS too_deep,
    EXISTS (
        SELECT 1
        FROM agent_task_queue task
        JOIN provider_failure_recovery recovery ON recovery.recovery_wakeup_id::text = task.context->>'wakeup_id'
        WHERE task.id = @task_id
          AND CASE WHEN task.context->>'wakeup_revision' ~ '^[0-9]{1,18}$'
                   THEN (task.context->>'wakeup_revision')::bigint ELSE 0 END
              >= COALESCE(recovery.recovery_wakeup_revision, 0)
    ) AS wakeup_lineage;

-- CHE-1736 M1.2 fence for a held provider retry. TRUE only if the failed task is
-- still the issue's latest run, the issue is not in a terminal status, and the
-- issue is still assigned to the task's agent or squad. An unassigned issue, a
-- newer run or a reassignment means no retry.
-- name: GetProviderHeldRetryFence :one
SELECT (
    i.id IS NOT NULL
    AND NOT EXISTS (
        SELECT 1 FROM agent_task_queue newer
        WHERE newer.issue_id = t.issue_id AND newer.id <> t.id
          AND (newer.created_at, newer.id) > (t.created_at, t.id)
    )
    AND i.status NOT IN ('done', 'cancelled')
    AND NOT EXISTS (
        SELECT 1 FROM issue_status s
        WHERE s.workspace_id = i.workspace_id AND s.key = i.status AND s.category IN ('done', 'closed')
    )
    AND (
        (i.assignee_type = 'agent' AND i.assignee_id = t.agent_id)
        OR (i.assignee_type = 'squad' AND i.assignee_id = t.squad_id)
    )
)::boolean AS allowed
FROM agent_task_queue t
LEFT JOIN issue i ON i.id = t.issue_id
WHERE t.id = @task_id;
