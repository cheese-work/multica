-- name: ListDecisionRequestCards :many
SELECT request.id AS request_id, message.id AS message_id, request.issue_id,
    COALESCE(workspace.issue_prefix, 'MUL')::text AS issue_prefix, issue.number AS issue_number,
    source.id AS source_comment_id, request.ask, request.status, request.created_at,
    request.withdraw_reason, request.cancel_reason, request.options,
    COALESCE(request.withdrawn_at, request.cancel_notice_posted_at) AS withdrawn_at,
    record.issue_comment_id AS record_comment_id
FROM decision_request request
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
JOIN workspace ON workspace.id = request.workspace_id
JOIN chat_message message ON message.chat_session_id = request.chat_session_id
    AND (message.id = request.ping_message_id OR request.id = ANY(message.decision_request_ids))
LEFT JOIN comment source ON source.id = request.source_comment_id
    AND source.workspace_id = request.workspace_id AND source.issue_id = request.issue_id AND source.deleted_at IS NULL
LEFT JOIN decision_answer record ON record.decision_request_id = request.id AND request.status = 'answered'
    AND record.disclaimed_at IS NULL
WHERE request.workspace_id = sqlc.arg('workspace_id')
    AND message.id = ANY(sqlc.arg('message_ids')::uuid[])
ORDER BY message.id, request.created_at, request.id;

-- name: ListOpenDecisionRequestsForHuman :many
SELECT request.id, request.issue_id,
    (COALESCE(NULLIF(workspace.issue_prefix, ''), 'MUL') || '-' || issue.number::text)::text AS issue_identifier,
    request.asking_agent_id, COALESCE(agent.name, '')::text AS asking_agent_name,
    request.ask, request.escalation_class, request.created_at,
    request.chat_session_id, request.ping_message_id AS message_id
FROM decision_request request
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
JOIN workspace ON workspace.id = request.workspace_id
LEFT JOIN agent ON agent.id = request.asking_agent_id AND agent.workspace_id = request.workspace_id
WHERE request.workspace_id = sqlc.arg('workspace_id')
    AND request.human_user_id = sqlc.arg('human_user_id')
    AND request.status = 'open'
ORDER BY request.created_at, request.id;

-- name: StampDecisionCancelMessageRequests :exec
UPDATE chat_message SET decision_request_ids = sqlc.arg('request_ids')::uuid[]
WHERE id = sqlc.arg('id') AND chat_session_id = sqlc.arg('chat_session_id');

-- name: GetDecisionCancelMessageForRequest :one
SELECT * FROM chat_message
WHERE chat_session_id = sqlc.arg('chat_session_id')
    AND sqlc.arg('request_id')::uuid = ANY(decision_request_ids)
    AND message_kind = 'decision_cancel_notice'
ORDER BY created_at DESC, id DESC LIMIT 1;
