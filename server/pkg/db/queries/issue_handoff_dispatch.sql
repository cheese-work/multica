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
