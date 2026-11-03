-- name: ListAskRecoveryEvents :many
-- Open block events whose source task has ended and that no earlier pass sent
-- to repair: the work for the recovery notice.
SELECT event.* FROM ask_guard_event event
JOIN agent_task_queue task ON task.id = event.task_id AND task.status IN ('completed', 'failed', 'cancelled')
WHERE event.mode = 'block' AND event.cleared_at IS NULL AND event.repair_reason IS NULL
ORDER BY event.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: LockAskGuardEvent :one
SELECT * FROM ask_guard_event WHERE id = $1 AND workspace_id = $2 FOR UPDATE;

-- name: GetAskGuardEventSource :one
-- The stored source of an event: the text kept with the event, else the intent
-- comment of an intent target. Empty when the source was deleted.
SELECT COALESCE(event.source_text, comment.content, '')::text AS source
FROM ask_guard_event event
LEFT JOIN decision_intent intent ON intent.id = event.intent_id AND intent.workspace_id = event.workspace_id
LEFT JOIN comment ON comment.id = intent.comment_id AND comment.issue_id = event.issue_id AND comment.workspace_id = event.workspace_id
WHERE event.id = $1 AND event.workspace_id = $2;

-- name: SetAskGuardEventRepair :exec
UPDATE ask_guard_event SET repair_reason = $3
WHERE id = $1 AND workspace_id = $2 AND cleared_at IS NULL AND repair_reason IS NULL;

-- name: MarkAskGuardEventDelivered :execrows
UPDATE ask_guard_event SET cleared_at = clock_timestamp(), delivered_at = clock_timestamp()
WHERE id = $1 AND workspace_id = $2 AND cleared_at IS NULL;

-- name: InsertAskRecoveryNotice :one
INSERT INTO ask_recovery_notice (id, workspace_id, issue_id, event_id, member_id, chat_session_id, chat_message_id, comment_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (event_id) DO NOTHING
RETURNING *;

-- name: GetAskRecoveryNoticeByComment :one
SELECT * FROM ask_recovery_notice WHERE comment_id = $1 AND workspace_id = $2;

-- name: ListAskRecoveryNoticesByMessage :many
SELECT * FROM ask_recovery_notice WHERE chat_message_id = ANY($1::uuid[]) AND workspace_id = $2;

-- name: MarkAskRecoveryNoticeAnswered :exec
UPDATE ask_recovery_notice SET state = 'answered', answered_at = clock_timestamp()
WHERE id = $1 AND state = 'delivered';
