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
