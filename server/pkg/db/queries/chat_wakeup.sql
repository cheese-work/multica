-- name: CreateChatWakeup :one
INSERT INTO chat_wakeup (workspace_id, chat_session_id, agent_id, title, instruction, fire_at, created_by_task_id,
    issue_id, event_type, filter_actor_type, filter_actor_id, until_status, on_timeout)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: CountPendingChatWakeups :one
SELECT count(*) FROM chat_wakeup
WHERE chat_session_id = $1 AND status = 'pending';

-- name: ListPendingChatWakeups :many
SELECT * FROM chat_wakeup
WHERE chat_session_id = $1 AND status = 'pending'
ORDER BY fire_at, id;

-- name: CancelChatWakeup :one
UPDATE chat_wakeup SET status = 'cancelled', resolved_at = now()
WHERE id = $1 AND chat_session_id = $2 AND status = 'pending'
RETURNING *;

-- name: ListDueChatWakeups :many
SELECT * FROM chat_wakeup
WHERE status = 'pending' AND fire_at <= now()
ORDER BY fire_at, id
LIMIT $1;

-- ClaimChatWakeup is the fire gate: only one caller flips a pending row, and it
-- holds the row lock until its transaction ends. An issue watch may be claimed
-- before fire_at (its expiry) when its event arrived.
-- name: ClaimChatWakeup :one
UPDATE chat_wakeup SET status = 'fired', resolved_at = now()
WHERE id = $1 AND status = 'pending' AND (fire_at <= now() OR issue_id IS NOT NULL)
RETURNING *;

-- name: SetChatWakeupFiredTask :exec
UPDATE chat_wakeup SET fired_task_id = $2 WHERE id = $1;

-- name: DropChatWakeup :exec
UPDATE chat_wakeup SET status = 'cancelled', resolved_at = now()
WHERE id = $1 AND status = 'pending';

-- name: ListChatWakeupsByFiredTasks :many
SELECT * FROM chat_wakeup
WHERE fired_task_id = ANY(@task_ids::uuid[]);

-- name: ListPendingChatIssueWatches :many
SELECT * FROM chat_wakeup
WHERE status = 'pending' AND issue_id IS NOT NULL
  AND (created_at, id) > (@after_created_at::timestamptz, @after_id::uuid)
ORDER BY created_at, id
LIMIT @page_size;

-- First comment on the issue after the watch was set that passes the actor filter.
-- name: FirstMatchingIssueComment :one
SELECT id, author_type, author_id, content FROM comment
WHERE issue_id = @issue_id AND type = 'comment' AND created_at > @since
  AND (@actor_type::text = '' OR author_type = @actor_type)
  AND (@actor_id::uuid IS NULL OR author_id = @actor_id)
ORDER BY created_at, id
LIMIT 1;

-- name: StampChatTaskWatchSource :exec
-- The DM turn a watch fires reads the matched comment, so the task names it as
-- its trigger evidence and takes its taint, read here in the task's own
-- transaction (the task trigger unions it with what the task has). Taint only
-- grows, so this is never older than what the watch read.
UPDATE agent_task_queue SET
    taint_ids = COALESCE((SELECT c.taint_ids FROM comment c WHERE c.id = sqlc.arg('comment_id')::uuid), '{}'),
    trigger_evidence_kind = 'comment',
    trigger_evidence_ref_id = sqlc.arg('comment_id')::uuid
WHERE id = sqlc.arg('task_id')::uuid;
