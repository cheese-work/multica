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

-- PR lifecycle (CHE-2298 c1).

-- name: ListPROnlyRunIDs :many
-- Unfinished runs of the issue that exist only to act on this PR: every wakeup
-- receipt that started the run is a PR wake for the PR, and the PR's own
-- pr.merged wake is not among them (it is the work a merge asks for).
-- @key_prefix is 'github:<owner>/<repo>#<number>', lower-cased. The a3 bridge's
-- pr.event receipts carry the PR in the payload, not the key; a merged or closed
-- cause is the work the merge or close asks for, so it is not PR-only.
SELECT t.id
FROM agent_task_queue t
WHERE t.issue_id = @issue_id
  AND t.status IN ('queued', 'dispatched', 'running')
  AND EXISTS (SELECT 1 FROM issue_wakeup_receipt r WHERE r.task_id = t.id)
  AND NOT EXISTS (
      SELECT 1 FROM issue_wakeup_receipt r
      WHERE r.task_id = t.id
        AND NOT (r.event_type IN ('pr.checks_failed', 'pr.checks_passed', 'pr.mergeability', 'pr.returned_to_draft')
                 AND (r.event_key = @key_prefix::text
                      OR left(r.event_key, length(@key_prefix::text) + 1) = @key_prefix::text || ':'))
        AND NOT (r.event_type = 'pr.event'
                 AND r.payload->>'kind' NOT IN ('merged', 'closed')
                 AND lower('github:' || (r.payload->>'repo_owner') || '/' || (r.payload->>'repo_name') || '#' || (r.payload->>'pr_number')) = @key_prefix::text)
  )
ORDER BY t.created_at;

-- name: SupersedeOpenPREventsForPR :execrows
-- A merged or closed PR has nothing left to fix: undelivered conflict, head,
-- check, draft and ready events of the PR are superseded, and their pending
-- deliveries fail. A delivery already in flight (delivering) is left to finish.
WITH failed AS (
    UPDATE task_pr_event_delivery d
    SET status = 'failed', failure_reason = 'pr_closed', updated_at = now()
    FROM issue_pr_event e
    WHERE e.id = d.event_id
      AND e.issue_id = @issue_id
      AND e.pull_request_id = @pull_request_id
      AND e.kind NOT IN ('merged', 'closed')
      AND d.status = 'pending'
    RETURNING d.event_id
)
UPDATE issue_pr_event e
SET status = 'superseded'
WHERE e.issue_id = @issue_id
  AND e.pull_request_id = @pull_request_id
  AND e.kind NOT IN ('merged', 'closed')
  AND (e.status IN ('accepted', 'next_turn')
       OR e.id IN (SELECT event_id FROM failed));

-- name: ListAcceptedPRNoticeEvents :many
-- Draft and ready events of the PR still waiting for a live run to take them.
SELECT id FROM issue_pr_event
WHERE issue_id = @issue_id
  AND pull_request_id = @pull_request_id
  AND kind IN ('draft', 'ready')
  AND status = 'accepted'
ORDER BY observed_at, created_at;

-- name: ListRunningPREventTaskIDs :many
-- Running runs of the issue that negotiated typed PR-event delivery.
SELECT t.id
FROM agent_task_queue t
JOIN task_pr_event_capability cap ON cap.task_id = t.id
WHERE t.issue_id = @issue_id
  AND t.status = 'running'
  AND cap.capability = 'task-supplement-pr-event-v1'
ORDER BY t.created_at;
