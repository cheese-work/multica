-- name: GetDeliveredChatTaskSupplement :one
-- A steered message the provider holds for this exact run. Pending, delivering,
-- requeued and withdrawn receipts do not match: the agent may not have seen them.
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
