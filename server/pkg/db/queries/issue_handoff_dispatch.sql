-- name: CreateHandoffDispatch :one
INSERT INTO issue_handoff_dispatch (
    id, workspace_id, issue_id, status_history_id, target_type, target_id,
    actor_type, actor_id, source_task_id, context_comment_ids, handoff_write
) VALUES (
    @id, @workspace_id, @issue_id, sqlc.narg(status_history_id), @target_type, @target_id,
    @actor_type, sqlc.narg(actor_id), sqlc.narg(source_task_id), @context_comment_ids::uuid[], @handoff_write
)
RETURNING *;

-- name: GetHandoffDispatch :one
SELECT * FROM issue_handoff_dispatch WHERE id = $1;

-- name: LockHandoffDispatch :one
SELECT * FROM issue_handoff_dispatch WHERE id = $1 FOR UPDATE;

-- name: GetTaskByHandoffDispatch :one
SELECT id FROM agent_task_queue WHERE handoff_dispatch_id = $1;

-- name: LinkTaskToHandoffDispatch :exec
-- A run already linked to another intent keeps its link.
UPDATE agent_task_queue SET handoff_dispatch_id = @dispatch_id WHERE id = @task_id AND handoff_dispatch_id IS NULL;

-- name: SettleHandoffDispatch :one
-- Only a pending intent settles; a terminal state has no exit.
UPDATE issue_handoff_dispatch
SET state = @state, task_id = sqlc.narg(task_id), skip_reason = sqlc.narg(skip_reason)
WHERE id = @id AND state = 'pending'
RETURNING *;

-- name: LockHandoffThread :exec
-- Takes the issue's handoff lock until commit, only for a task that has a
-- handoff edge on the issue: two first posts of one chain take turns (CHE-6121).
-- Lock order: this lock, then the thread lock (LockCommentThread).
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(issue_id)::uuid::text || ':handoff_thread', 0))
WHERE EXISTS (
    SELECT 1 FROM issue_handoff_dispatch d
    WHERE d.issue_id = sqlc.arg(issue_id) AND d.workspace_id = sqlc.arg(workspace_id)
      AND d.source_task_id IS NOT NULL AND d.task_id IS NOT NULL
      AND (d.source_task_id = sqlc.arg(task_id) OR d.task_id = sqlc.arg(task_id))
);

-- name: GetNewestHandoffChainComment :one
-- The newest chain comment of a task on the issue. The chain is the task plus
-- every task reached through handoff edges (a dispatch row with both a mover and
-- a started run), in either direction. No row when the chain is only the task,
-- or when it has no eligible comment: a deleted or decision comment is skipped.
WITH RECURSIVE chain(task_id) AS (
    SELECT sqlc.arg(task_id)::uuid
    UNION
    SELECT CASE WHEN d.source_task_id = c.task_id THEN d.task_id ELSE d.source_task_id END
    FROM chain c
    JOIN issue_handoff_dispatch d
      ON d.issue_id = sqlc.arg(issue_id) AND d.workspace_id = sqlc.arg(workspace_id)
     AND d.source_task_id IS NOT NULL AND d.task_id IS NOT NULL
     AND (d.source_task_id = c.task_id OR d.task_id = c.task_id)
)
SELECT cm.id FROM comment cm
WHERE cm.issue_id = sqlc.arg(issue_id) AND cm.workspace_id = sqlc.arg(workspace_id)
  AND cm.source_task_id IN (SELECT task_id FROM chain)
  AND (SELECT count(*) FROM chain) > 1
  AND cm.type = 'comment' AND cm.deleted_at IS NULL
  AND cm.decision_request_id IS NULL AND cm.decision_comment_kind IS NULL
ORDER BY cm.created_at DESC, cm.id DESC
LIMIT 1;

-- name: ClaimDueHandoffDispatches :many
-- The recovery sweep's claim (CHE-5488). Counts the attempt and pushes
-- next_attempt_at out by its backoff (30 s, 30 s, then 60 s), so the claim is
-- also the lease: another sweeper skips the row until the backoff ends.
UPDATE issue_handoff_dispatch
SET attempts = attempts + 1,
    next_attempt_at = sqlc.arg(now)::timestamptz + (CASE WHEN attempts < 2 THEN 30 ELSE 60 END) * interval '1 second'
WHERE id IN (
    SELECT d.id FROM issue_handoff_dispatch d
    WHERE d.state = 'pending'
      AND d.created_at <= sqlc.arg(created_before)::timestamptz
      AND d.next_attempt_at <= sqlc.arg(now)::timestamptz
    ORDER BY d.created_at
    LIMIT @batch_size
    FOR UPDATE SKIP LOCKED
)
RETURNING *;

-- name: HandoffDispatchSuperseded :one
-- True when the issue is no longer where the intent's write left it: gone,
-- handed to someone else, or moved again since the intent's status-history row.
SELECT
    NOT EXISTS (SELECT 1 FROM issue i WHERE i.id = @issue_id
                  AND i.assignee_type = sqlc.arg(target_type)::text AND i.assignee_id = @target_id)
    OR EXISTS (SELECT 1 FROM activity_log a
               WHERE a.issue_id = @issue_id AND a.action = 'status_changed'
                 AND a.id IS DISTINCT FROM sqlc.narg(status_history_id)::uuid
                 AND a.created_at > COALESCE(
                     (SELECT h.created_at FROM activity_log h WHERE h.id = sqlc.narg(status_history_id)::uuid),
                     @created_at::timestamptz))
    OR EXISTS (SELECT 1 FROM activity_log h JOIN issue i ON i.id = h.issue_id
               WHERE h.id = sqlc.narg(status_history_id)::uuid
                 AND h.details->>'to' IS NOT NULL AND h.details->>'to' <> i.status)
    AS superseded;
