-- name: CountPendingChatWakeups :one
SELECT count(*) FROM chat_wakeup WHERE chat_session_id = $1 AND status = 'pending';

-- name: CreateChatWakeup :one
INSERT INTO chat_wakeup (workspace_id, chat_session_id, agent_id, created_by, source_task_id, instruction, fire_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListPendingChatWakeups :many
SELECT * FROM chat_wakeup
WHERE chat_session_id = $1 AND status = 'pending'
ORDER BY fire_at ASC, id ASC;

-- name: CancelChatWakeup :one
-- Only a pending wakeup cancels; once fired, the turn it started is an
-- ordinary task. Scoped to the DM so one task cannot cancel another DM's.
UPDATE chat_wakeup SET status = 'cancelled', updated_at = now()
WHERE id = $1 AND chat_session_id = $2 AND status = 'pending'
RETURNING *;

-- name: ListDueChatWakeups :many
SELECT * FROM chat_wakeup
WHERE status = 'pending' AND fire_at <= now()
ORDER BY fire_at ASC, id ASC
LIMIT $1;

-- name: LockPendingChatWakeup :one
-- Re-reads a due wakeup under a row lock; a concurrent cancel or tick wins.
SELECT * FROM chat_wakeup
WHERE id = $1 AND status = 'pending' AND fire_at <= now()
FOR UPDATE SKIP LOCKED;

-- name: MoveChatWakeupToSession :exec
UPDATE chat_wakeup SET chat_session_id = $2, updated_at = now() WHERE id = $1;

-- name: SettleChatWakeup :exec
-- status is fired (task_id set) or expired (the DM or agent can no longer take a turn).
UPDATE chat_wakeup SET status = $2, task_id = sqlc.narg('task_id'), updated_at = now() WHERE id = $1;

-- name: DeleteChatWakeupsBySession :exec
DELETE FROM chat_wakeup WHERE chat_session_id = $1;

-- name: MarkChatTaskWakeup :one
-- Flags a chat task as the turn a DM wakeup started.
UPDATE agent_task_queue SET task_kind = 'wakeup' WHERE id = $1 RETURNING *;

-- name: MoveQueuedWakeupTasksToSession :exec
-- A continuation archives the DM the run was replying in. A wakeup turn that is
-- queued but not started in that DM would run, and notify nobody, in an
-- archived session, so it moves with the conversation: the task, its hidden
-- input and the wakeup row that started it.
WITH moved AS (
    UPDATE agent_task_queue SET chat_session_id = sqlc.arg('to_session_id')
    WHERE agent_task_queue.chat_session_id = sqlc.arg('from_session_id') AND agent_task_queue.task_kind = 'wakeup' AND agent_task_queue.status IN ('queued', 'deferred')
    RETURNING id
), moved_inputs AS (
    UPDATE chat_message SET chat_session_id = sqlc.arg('to_session_id')
    WHERE chat_message.message_kind = 'wakeup_trigger' AND chat_message.task_id IN (SELECT moved.id FROM moved)
    RETURNING id
)
UPDATE chat_wakeup SET chat_session_id = sqlc.arg('to_session_id'), updated_at = now()
WHERE chat_wakeup.task_id IN (SELECT moved.id FROM moved);
