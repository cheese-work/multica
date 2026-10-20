-- name: CountPendingChatWakeups :one
-- Counts the DM and the DMs it was continued from: a wakeup set before a
-- continuation is still pending after it, so it still holds a slot.
WITH RECURSIVE lineage AS (
    SELECT cs.id, cs.continued_from_session_id, 1 AS depth FROM chat_session cs WHERE cs.id = $1
    UNION ALL
    SELECT s.id, s.continued_from_session_id, l.depth + 1
    FROM chat_session s JOIN lineage l ON s.id = l.continued_from_session_id
    WHERE l.depth < 16
)
SELECT count(*) FROM chat_wakeup WHERE chat_session_id IN (SELECT lineage.id FROM lineage) AND status = 'pending';

-- name: CreateChatWakeup :one
INSERT INTO chat_wakeup (workspace_id, chat_session_id, agent_id, created_by, source_task_id, instruction, fire_at, condition)
VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.narg('condition'))
RETURNING *;

-- name: ListPendingChatWakeups :many
-- The DM's pending wakeups, including those set in the DMs it was continued from.
WITH RECURSIVE lineage AS (
    SELECT cs.id, cs.continued_from_session_id, 1 AS depth FROM chat_session cs WHERE cs.id = $1
    UNION ALL
    SELECT s.id, s.continued_from_session_id, l.depth + 1
    FROM chat_session s JOIN lineage l ON s.id = l.continued_from_session_id
    WHERE l.depth < 16
)
SELECT * FROM chat_wakeup
WHERE chat_session_id IN (SELECT lineage.id FROM lineage) AND status = 'pending'
ORDER BY fire_at ASC, chat_wakeup.id ASC;

-- name: CancelChatWakeup :one
-- Only a pending wakeup cancels; once fired, the turn it started is an
-- ordinary task. Scoped to the DM and its continuation lineage, so one DM
-- cannot cancel an unrelated DM's wakeup.
WITH RECURSIVE lineage AS (
    SELECT cs.id, cs.continued_from_session_id, 1 AS depth FROM chat_session cs WHERE cs.id = sqlc.arg('chat_session_id')
    UNION ALL
    SELECT s.id, s.continued_from_session_id, l.depth + 1
    FROM chat_session s JOIN lineage l ON s.id = l.continued_from_session_id
    WHERE l.depth < 16
)
UPDATE chat_wakeup SET status = 'cancelled', updated_at = now()
WHERE chat_wakeup.id = sqlc.arg('wakeup_id') AND chat_session_id IN (SELECT lineage.id FROM lineage) AND status = 'pending'
RETURNING *;

-- name: ListDueChatWakeups :many
SELECT * FROM chat_wakeup
WHERE status = 'pending' AND fire_at <= now()
ORDER BY fire_at ASC, id ASC
LIMIT $1;

-- name: LockPendingChatWakeup :one
-- Re-reads a pending wakeup under a row lock; a concurrent cancel or tick wins.
-- The caller decides it is due: the deadline passed or its condition held.
SELECT * FROM chat_wakeup
WHERE id = $1 AND status = 'pending'
FOR UPDATE SKIP LOCKED;

-- name: ListWaitingConditionalChatWakeups :many
-- Pending wakeups whose condition is still being watched (before the deadline),
-- least recently checked first. seen holds the ids already evaluated in this
-- tick, so a tick can page through all of them.
SELECT * FROM chat_wakeup
WHERE status = 'pending' AND condition IS NOT NULL AND fire_at > now()
  AND NOT (id = ANY(COALESCE(sqlc.arg('seen')::uuid[], '{}'::uuid[])))
ORDER BY condition_checked_at ASC NULLS FIRST, fire_at ASC, id ASC
LIMIT sqlc.arg('batch');

-- name: TouchChatWakeupConditionChecked :exec
UPDATE chat_wakeup SET condition_checked_at = now() WHERE id = $1;

-- name: UpdateChatWakeupCondition :exec
UPDATE chat_wakeup SET condition = $2, updated_at = now() WHERE id = $1 AND status = 'pending';

-- name: ListChatWakeupLinkedPullRequests :many
SELECT pr.pr_number, COALESCE(pr.snapshot_head_sha, '')::text AS head_sha, COALESCE(pr.checks_rollup_state, '')::text AS checks
FROM github_pull_request pr
JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = $1;

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

-- name: GetChatWakeupIssueIdentifier :one
SELECT (ws.issue_prefix || '-' || i.number)::text AS identifier
FROM issue i JOIN workspace ws ON ws.id = i.workspace_id
WHERE i.id = $1 AND i.workspace_id = $2;

-- name: GetChatWakeupNewComment :one
-- The newest comment the watched actor wrote on the issue after the wakeup was set.
SELECT id FROM comment
WHERE issue_id = $1 AND workspace_id = $2 AND author_type = $3 AND author_id = $4 AND created_at > $5
ORDER BY created_at DESC LIMIT 1;

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
