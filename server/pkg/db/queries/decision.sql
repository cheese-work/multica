-- name: CreateDecisionIntent :one
WITH stamp AS (SELECT clock_timestamp() AS opened_at)
INSERT INTO decision_intent (workspace_id, issue_id, task_id, agent_id, comment_id, opened_at, expires_at)
SELECT sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('task_id'), sqlc.arg('agent_id'), sqlc.arg('comment_id'), opened_at, opened_at + interval '3 minutes'
FROM stamp
RETURNING *;

-- name: CreateDecisionIntentTarget :one
INSERT INTO decision_intent_target (decision_intent_id, human_user_id)
VALUES (sqlc.arg('decision_intent_id'), sqlc.arg('human_user_id'))
RETURNING *;

-- name: CreateDecisionRequest :one
INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, ping_message_id, issue_comment_id, ask, escalation_class, origin, decision_intent_id)
VALUES (sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('asking_agent_id'), sqlc.arg('human_user_id'), sqlc.arg('chat_session_id'), sqlc.narg('ping_message_id'), sqlc.narg('issue_comment_id'), sqlc.arg('ask'), sqlc.arg('escalation_class'), sqlc.arg('origin'), sqlc.narg('decision_intent_id'))
RETURNING *;

-- name: CreateDecisionAnswer :one
INSERT INTO decision_answer (decision_request_id, answered_by_task_id, source_chat_session_id, chat_message_ids, content_snapshot, effective_at, issue_comment_id)
VALUES (sqlc.arg('decision_request_id'), sqlc.arg('answered_by_task_id'), sqlc.arg('source_chat_session_id'), sqlc.arg('chat_message_ids'), sqlc.arg('content_snapshot'), sqlc.arg('effective_at'), sqlc.narg('issue_comment_id'))
RETURNING *;

-- name: GetDecisionRequest :one
SELECT * FROM decision_request
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id');

-- name: ListDecisionRequestsByIssue :many
SELECT * FROM decision_request
WHERE workspace_id = sqlc.arg('workspace_id') AND issue_id = sqlc.arg('issue_id')
ORDER BY created_at, id;

-- name: LockDecisionSession :one
SELECT id FROM chat_session
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE;

-- name: LockDecisionIssue :one
SELECT id FROM issue
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR NO KEY UPDATE;

-- name: LockDecisionCallingTask :one
SELECT task.id FROM agent_task_queue task
JOIN agent owner ON owner.id = task.agent_id
WHERE owner.workspace_id = sqlc.arg('workspace_id') AND task.id = sqlc.arg('id')
FOR SHARE OF task NOWAIT;

-- name: LockDecisionDeliveryTask :one
SELECT task.id FROM agent_task_queue task
JOIN agent owner ON owner.id = task.agent_id
WHERE owner.workspace_id = sqlc.arg('workspace_id') AND task.id = sqlc.arg('id')
FOR UPDATE OF task NOWAIT;

-- name: LockDecisionAskingAgent :one
SELECT id FROM agent
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR SHARE NOWAIT;

-- name: LockDecisionMember :one
SELECT id FROM member
WHERE workspace_id = sqlc.arg('workspace_id') AND user_id = sqlc.arg('user_id')
FOR KEY SHARE NOWAIT;

-- name: LockDecisionSourceMessages :many
SELECT message.id FROM chat_message message
JOIN chat_session session ON session.id = message.chat_session_id
WHERE session.workspace_id = sqlc.arg('workspace_id') AND message.id = ANY(sqlc.arg('message_ids')::uuid[])
ORDER BY message.id
FOR KEY SHARE OF message NOWAIT;

-- name: LockDecisionIntent :one
SELECT * FROM decision_intent
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE;

-- name: LockDecisionPendingIntent :one
SELECT * FROM decision_intent
WHERE workspace_id = sqlc.arg('workspace_id') AND task_id = sqlc.arg('task_id')
  AND consumed_at IS NULL AND closed_at IS NULL
FOR UPDATE;

-- name: LockDecisionIntentTarget :one
SELECT target.* FROM decision_intent_target target
JOIN decision_intent intent ON intent.id = target.decision_intent_id
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND target.decision_intent_id = sqlc.arg('decision_intent_id') AND target.human_user_id = sqlc.arg('human_user_id')
FOR UPDATE OF target;

-- name: LockDecisionRequest :one
SELECT * FROM decision_request
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE;

-- name: LockDecisionAnswer :one
SELECT answer.* FROM decision_answer answer
JOIN decision_request request ON request.id = answer.decision_request_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND answer.id = sqlc.arg('id')
FOR UPDATE OF answer;
