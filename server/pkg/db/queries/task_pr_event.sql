-- Typed PR-event delivery to a live run (CHE-2298 a2). Separate from
-- task_supplement: that path joins comment and labels text as human guidance.

-- name: RecordTaskPREventCapability :execrows
-- Called in the start transaction, after the task is running. A missing row is
-- the fail-closed value for old daemons, old servers and unsupported providers.
INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability)
SELECT t.id, r.workspace_id, t.issue_id, 'task-supplement-pr-event-v1'
FROM agent_task_queue t
JOIN agent_runtime r ON r.id = t.runtime_id
WHERE t.id = @task_id
  AND t.status = 'running'
  AND t.issue_id IS NOT NULL
  AND r.provider IN ('codex', 'claude', 'grok', 'cursor', 'kiro')
ON CONFLICT DO NOTHING;

-- name: GetTaskPREventCapability :one
SELECT * FROM task_pr_event_capability WHERE task_id = @task_id;

-- name: EnqueueTaskPREventDelivery :one
-- Locking the exact task serializes against terminal transitions: a run that
-- ended first gets no row. Only an accepted event of the task's own issue is
-- queued; it moves to delivering with the row.
WITH locked_task AS MATERIALIZED (
    SELECT t.id, t.issue_id
    FROM agent_task_queue t
    JOIN task_pr_event_capability cap ON cap.task_id = t.id
    WHERE t.id = @task_id
      AND t.status = 'running'
      AND cap.capability = 'task-supplement-pr-event-v1'
    FOR UPDATE OF t
), claimed_event AS (
    UPDATE issue_pr_event e
    SET status = 'delivering'
    FROM locked_task t
    WHERE e.id = @event_id
      AND e.issue_id = t.issue_id
      AND e.status = 'accepted'
    RETURNING e.id, e.workspace_id, e.issue_id
)
INSERT INTO task_pr_event_delivery (task_id, event_id, workspace_id, issue_id, status)
SELECT @task_id, e.id, e.workspace_id, e.issue_id, 'pending'
FROM claimed_event e
ON CONFLICT DO NOTHING
RETURNING *;

-- name: ClaimNextTaskPREvent :one
-- Structured fields only: no PR title, body or comment text, no member author.
-- old_head_sha is the previously observed head of a head_changed event;
-- check_name is the first failing check of a checks_failed event's head.
WITH next AS MATERIALIZED (
    SELECT d.event_id, d.task_id
    FROM task_pr_event_delivery d
    JOIN agent_task_queue t ON t.id = d.task_id
    JOIN task_pr_event_capability cap ON cap.task_id = t.id
    WHERE d.task_id = @task_id
      AND d.status = 'pending'
      AND t.status = 'running'
      AND cap.capability = 'task-supplement-pr-event-v1'
    ORDER BY d.created_at, d.event_id
    FOR UPDATE OF d SKIP LOCKED
    LIMIT 1
), claimed AS (
    UPDATE task_pr_event_delivery d
    SET status = 'delivering',
        attempt_count = attempt_count + 1,
        failure_reason = NULL,
        updated_at = now()
    FROM next
    WHERE d.event_id = next.event_id
      AND d.task_id = next.task_id
    RETURNING d.event_id, d.attempt_count
)
SELECT claimed.event_id, claimed.attempt_count, e.kind, e.head_sha, e.base_sha,
       pr.pr_number,
       COALESCE((
           SELECT prev.head_sha FROM issue_pr_event prev
           WHERE e.kind = 'head_changed'
             AND prev.issue_id = e.issue_id
             AND prev.pull_request_id = e.pull_request_id
             AND prev.head_sha <> e.head_sha
             AND prev.status <> 'superseded'
             AND prev.observed_at < e.observed_at
           ORDER BY prev.observed_at DESC
           LIMIT 1
       ), '')::text AS old_head_sha,
       COALESCE((
           SELECT cr.name FROM github_pull_request_check_run cr
           WHERE e.kind = 'checks_failed'
             AND cr.pr_id = e.pull_request_id
             AND cr.head_sha = e.head_sha
             AND cr.conclusion IN ('failure', 'timed_out', 'cancelled', 'action_required', 'startup_failure')
           ORDER BY cr.ordinal
           LIMIT 1
       ), '')::text AS check_name
FROM claimed
JOIN issue_pr_event e ON e.id = claimed.event_id
JOIN github_pull_request pr ON pr.id = e.pull_request_id;

-- name: AckTaskPREventDelivered :one
-- The provider can accept input before completion while its HTTP acknowledgement
-- arrives afterward. Serialize with settlement and preserve that successful write.
WITH locked_task AS MATERIALIZED (
    SELECT t.id FROM agent_task_queue t WHERE t.id = @task_id FOR UPDATE
), acked AS (
    UPDATE task_pr_event_delivery d
    SET status = 'delivered',
        delivered_at = COALESCE(delivered_at, now()),
        failure_reason = NULL,
        updated_at = now()
    FROM locked_task t
    WHERE d.task_id = t.id
      AND d.event_id = @event_id
      AND (d.status IN ('delivering', 'delivered')
           OR (d.status = 'failed' AND d.failure_reason = 'turn_ended' AND d.attempt_count > 0))
    RETURNING d.*
), moved AS (
    UPDATE issue_pr_event e
    SET status = 'delivered'
    FROM acked
    WHERE e.id = acked.event_id
)
SELECT * FROM acked;

-- name: AckTaskPREventFailed :one
-- A failed delivery returns its event to the next-turn path.
WITH failed AS (
    UPDATE task_pr_event_delivery
    SET status = 'failed',
        failure_reason = @failure_reason,
        updated_at = now()
    WHERE task_id = @task_id
      AND event_id = @event_id
      AND status = 'delivering'
    RETURNING *
), moved AS (
    UPDATE issue_pr_event e
    SET status = 'next_turn'
    FROM failed
    WHERE e.id = failed.event_id
      AND e.status = 'delivering'
)
SELECT * FROM failed;

-- name: SettleTerminalTaskPREventDeliveries :execrows
-- Same contract as SettleTerminalTaskSupplements: terminal transitions call this
-- in the task update's transaction. Undelivered events return to next_turn.
WITH settled AS (
    UPDATE task_pr_event_delivery AS d
    SET status = 'failed',
        failure_reason = 'turn_ended',
        updated_at = now()
    FROM agent_task_queue AS task
    WHERE d.task_id = task.id
      AND task.id = ANY(@task_ids::uuid[])
      AND task.status IN ('completed', 'failed', 'cancelled')
      AND d.status IN ('pending', 'delivering')
    RETURNING d.event_id
), moved AS (
    UPDATE issue_pr_event e
    SET status = 'next_turn'
    FROM settled
    WHERE e.id = settled.event_id
      AND e.status = 'delivering'
)
SELECT event_id FROM settled;
