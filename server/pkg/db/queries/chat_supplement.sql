-- name: CreateChatTaskSupplement :one
INSERT INTO chat_task_supplement (
    message_id, task_id, workspace_id, chat_session_id, author_id, status
) VALUES (
    @message_id, @task_id, @workspace_id, @chat_session_id, @author_id, 'pending'
)
RETURNING *;

-- name: GetLatestChatTaskSupplement :one
SELECT * FROM chat_task_supplement
WHERE chat_session_id = @chat_session_id AND message_id = @message_id
ORDER BY created_at DESC, task_id DESC
LIMIT 1;

-- name: LockLatestChatTaskSupplement :one
-- Callers hold the receipt's task row first (task row -> receipt), the same
-- order as claim, acknowledgement and terminal settlement.
SELECT * FROM chat_task_supplement
WHERE chat_session_id = @chat_session_id AND message_id = @message_id
ORDER BY created_at DESC, task_id DESC
LIMIT 1
FOR UPDATE;

-- name: ListChatTaskSupplementsByMessageIDs :many
-- A message is steered at most once per run; the latest receipt is its state.
SELECT DISTINCT ON (message_id) *
FROM chat_task_supplement
WHERE workspace_id = @workspace_id
  AND message_id = ANY(@message_ids::uuid[])
ORDER BY message_id, created_at DESC, task_id DESC;

-- name: ListResentUncertainChatMessageIDs :many
SELECT message_id FROM chat_task_supplement
WHERE resent_task_id = @task_id
  AND resent_uncertain
  AND message_id = ANY(@message_ids::uuid[]);

-- name: ClaimNextChatTaskSupplement :one
-- delivering is written under the task-row lock, so terminal settlement (which
-- updates that row first) can never label a claimed receipt as never claimed.
WITH task AS MATERIALIZED (
    SELECT t.id FROM agent_task_queue t
    JOIN task_supplement_capability cap ON cap.task_id = t.id
    WHERE t.id = @task_id
      AND t.status = 'running'
      AND cap.capability = 'task-supplement-chat-v1'
    FOR UPDATE OF t
), next AS MATERIALIZED (
    SELECT s.message_id, s.task_id
    FROM chat_task_supplement s
    JOIN task ON task.id = s.task_id
    WHERE s.status = 'pending'
    ORDER BY s.created_at, s.message_id
    LIMIT 1
    FOR UPDATE OF s
), claimed AS (
    UPDATE chat_task_supplement s
    SET status = 'delivering',
        attempt_count = attempt_count + 1,
        updated_at = now()
    FROM next
    WHERE s.message_id = next.message_id
      AND s.task_id = next.task_id
    RETURNING s.*
)
SELECT claimed.message_id, claimed.attempt_count, claimed.workspace_id,
       claimed.chat_session_id, m.content,
       COALESCE(NULLIF(btrim(u.name), ''), 'a user')::text AS author_name
FROM claimed
JOIN chat_message m ON m.id = claimed.message_id
LEFT JOIN "user" u ON u.id = claimed.author_id;

-- name: AckChatTaskSupplementDelivered :one
-- A positive ack is the fact that the provider holds the text. It is recorded
-- even after settlement or a confirmed resend: refusing it could not undo the
-- delivery, only hide a duplicate from the user.
WITH task AS MATERIALIZED (
    SELECT id FROM agent_task_queue WHERE id = @task_id FOR UPDATE
)
UPDATE chat_task_supplement s
SET status = 'delivered',
    delivered_at = COALESCE(s.delivered_at, now()),
    acked_after_resend_at = CASE WHEN s.resent_task_id IS NOT NULL
                                 THEN COALESCE(s.acked_after_resend_at, now())
                                 ELSE s.acked_after_resend_at END,
    updated_at = now()
FROM task t
WHERE s.task_id = t.id
  AND s.message_id = @message_id
  AND s.status IN ('delivering', 'uncertain', 'delivered')
RETURNING s.*;

-- name: AckChatTaskSupplementNotDelivered :one
-- The daemon cannot prove a rejection happened before the provider accepted
-- the text (a timeout can fire after acceptance), so a negative ack never
-- produces failed. A recorded delivery or a committed resend is left as it is.
WITH task AS MATERIALIZED (
    SELECT id FROM agent_task_queue WHERE id = @task_id FOR UPDATE
), target AS MATERIALIZED (
    SELECT s.message_id, s.task_id,
           (s.resent_task_id IS NULL AND s.status <> 'delivered') AS changes
    FROM chat_task_supplement s
    JOIN task ON task.id = s.task_id
    WHERE s.message_id = @message_id
      AND s.status IN ('delivering', 'uncertain', 'delivered')
    FOR UPDATE OF s
)
UPDATE chat_task_supplement s
SET status = CASE WHEN target.changes THEN 'uncertain' ELSE s.status END,
    failure_reason = CASE WHEN target.changes THEN @failure_reason::text ELSE s.failure_reason END,
    updated_at = CASE WHEN target.changes THEN now() ELSE s.updated_at END
FROM target
WHERE s.message_id = target.message_id
  AND s.task_id = target.task_id
RETURNING s.*;

-- name: SettleTerminalChatTaskSupplements :execrows
-- Runs in the terminal transition's transaction, after the task-row update.
UPDATE chat_task_supplement AS s
SET status = CASE WHEN s.status = 'pending' THEN 'failed' ELSE 'uncertain' END,
    failure_reason = CASE WHEN s.status = 'pending' THEN 'turn_ended' ELSE 'turn_ended_unconfirmed' END,
    updated_at = now()
FROM agent_task_queue AS task
WHERE s.task_id = task.id
  AND task.id = ANY(@task_ids::uuid[])
  AND task.status IN ('completed', 'failed', 'cancelled')
  AND s.status IN ('pending', 'delivering');

-- name: MarkChatTaskSupplementResent :one
UPDATE chat_task_supplement
SET resent_task_id = @resent_task_id,
    resent_uncertain = @resent_uncertain,
    updated_at = now()
WHERE message_id = @message_id
  AND task_id = @task_id
  AND resent_task_id IS NULL
RETURNING *;

-- name: DeleteChatTaskSupplementsBySession :exec
DELETE FROM chat_task_supplement
WHERE chat_session_id = @chat_session_id AND workspace_id = @workspace_id;

-- name: ChatTaskEndedSince :one
-- Tells a turn that just ended apart from a session that had no run at all.
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue
    WHERE chat_session_id = @chat_session_id
      AND id <> @exclude_task_id
      AND started_at IS NOT NULL
      AND status IN ('completed', 'failed', 'cancelled')
      AND completed_at >= @since
)::boolean;

-- name: GetDeliveredChatTaskSupplement :one
-- A steered message the provider holds for this exact run. Pending, delivering,
-- uncertain and failed receipts do not match: the agent may not have seen them.
SELECT * FROM chat_task_supplement
WHERE chat_session_id = @chat_session_id
  AND message_id = @message_id
  AND task_id = @task_id
  AND status = 'delivered';

-- name: ListDeliveredSteeredChatMessages :many
-- Delivered steered messages of a DM that no live decision answer quotes yet.
-- A steered message keeps task_id of its cancelled follow-up, so no turn lists
-- it as input.
SELECT m.* FROM chat_task_supplement s
JOIN chat_message m ON m.id = s.message_id AND m.chat_session_id = s.chat_session_id
WHERE s.chat_session_id = @chat_session_id
  AND s.workspace_id = @workspace_id
  AND s.status = 'delivered'
  AND m.created_at < @before
  AND NOT EXISTS (
      SELECT 1 FROM decision_answer a
      WHERE m.id = ANY(a.chat_message_ids) AND a.disclaimed_at IS NULL
  )
ORDER BY m.created_at DESC, m.id DESC
LIMIT @row_limit;
