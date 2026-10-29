-- name: LockDecisionSourceComment :one
SELECT * FROM comment
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id') AND deleted_at IS NULL
FOR SHARE NOWAIT;

-- name: FindDecisionRequestRetry :one
SELECT request.* FROM decision_request request
JOIN decision_intent intent ON intent.id = request.decision_intent_id AND intent.workspace_id = request.workspace_id
WHERE request.workspace_id = sqlc.arg('workspace_id')
  AND request.issue_id = sqlc.arg('issue_id') AND request.asking_agent_id = sqlc.arg('agent_id')
  AND intent.task_id = sqlc.arg('task_id') AND request.request_fingerprint = sqlc.arg('fingerprint')
  AND request.request_fingerprint <> '';

-- name: WithdrawOpenDecisionRequest :one
UPDATE decision_request
SET status = 'cancelled', cancel_reason = 'agent_withdrawn', withdraw_reason = sqlc.arg('reason'),
    withdrawn_at = clock_timestamp(), withdrawn_by_task_id = sqlc.narg('task_id'), withdrawn_by_user_id = sqlc.narg('user_id'),
    cancel_notice_posted_at = clock_timestamp()
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id') AND status = 'open'
RETURNING *;

-- name: GetDecisionAnswerReference :one
SELECT message.* FROM chat_message message
JOIN chat_session session ON session.id = message.chat_session_id
WHERE session.workspace_id = sqlc.arg('workspace_id') AND message.decision_answer_id = sqlc.arg('answer_id')
  AND message.message_kind = 'decision_reference'
ORDER BY message.created_at, message.id LIMIT 1;
