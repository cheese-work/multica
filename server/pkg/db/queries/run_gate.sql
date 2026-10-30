-- CHE-1918: shadow ledger of leader-run triggers (migration 670). Capture only.

-- name: UpsertRunGateEvent :one
-- One row per (source, issue, event, candidate). A repeat of the same event
-- (a comment re-triggered by an edit, a redelivered PR event) counts in
-- seen_count and leaves the first snapshot in place.
INSERT INTO run_gate_event (
    id, workspace_id, issue_id, source, event_key, candidate_key, origin, author_type,
    non_leader_agent_reply, trigger_comment_id, system_rule, wakeup_id, wakeup_revision,
    receipt_id, agent_id, squad_id, issue_revision, issue_status, pr_head_sha, event_time,
    native_fold_eligible, native_fold_task_id, queue_snapshot, active_task_id
) VALUES (
    @id, @workspace_id, @issue_id, @source, @event_key, @candidate_key, @origin, sqlc.narg('author_type'),
    @non_leader_agent_reply, sqlc.narg('trigger_comment_id'), sqlc.narg('system_rule'), sqlc.narg('wakeup_id'), sqlc.narg('wakeup_revision'),
    sqlc.narg('receipt_id'), sqlc.narg('agent_id'), sqlc.narg('squad_id'), @issue_revision, @issue_status, sqlc.narg('pr_head_sha'), @event_time,
    @native_fold_eligible, sqlc.narg('native_fold_task_id'), @queue_snapshot, sqlc.narg('active_task_id')
)
ON CONFLICT (source, issue_id, event_key, candidate_key)
DO UPDATE SET seen_count = run_gate_event.seen_count + 1, last_seen_at = now()
RETURNING id, seen_count;

-- name: RecordRunGateAdmission :exec
-- Copies the disposition the producer returned. Never derived from timing.
UPDATE run_gate_event
SET admission_status = @admission_status, admission_reason = @admission_reason, admission_at = now()
WHERE source = @source AND issue_id = @issue_id AND event_key = @event_key AND candidate_key = @candidate_key;

-- name: ListRunGateQueue :many
-- Live tasks of one agent on one issue: the same set the unique pending-task
-- index and the active-run checks look at.
SELECT id, status, is_leader_task, squad_id, COALESCE(context->>'head_sha', '')::text AS head_sha, trigger_comment_id,
       COALESCE(context->>'wakeup_id', '')::text AS wakeup_id, created_at
FROM agent_task_queue
WHERE issue_id = @issue_id AND agent_id = @agent_id
  AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
ORDER BY created_at DESC, id DESC
LIMIT 20;

-- name: FindRunGateCommentFoldTarget :one
-- Read-only mirror of the target MergeCommentIntoQueuedIssueTask would pick for
-- a comment trigger. Same predicate, no write, no lock.
SELECT t.id FROM agent_task_queue t
WHERE t.context->>'wakeup_id' IS NULL AND t.issue_id = @issue_id
  AND t.agent_id = @agent_id
  AND t.status = 'queued'
  AND t.is_leader_task = (sqlc.narg('squad_id')::uuid IS NOT NULL)
  AND t.squad_id IS NOT DISTINCT FROM sqlc.narg('squad_id')::uuid
  AND (
      COALESCE(sqlc.narg('head_sha')::text, '') = ''
      OR t.context->>'head_sha' = sqlc.narg('head_sha')::text
  )
ORDER BY t.created_at DESC
LIMIT 1;

-- name: FindRunGateWakeupFoldTarget :one
-- Read-only mirror of FindPendingWakeupTask for a queued run: the run whose
-- evidence a new input of the same wakeup replaces. A dispatched prompt is
-- immutable and is not a target.
SELECT id FROM agent_task_queue
WHERE context->>'wakeup_id' = @wakeup_id::text AND status = 'queued'
ORDER BY created_at
LIMIT 1;
