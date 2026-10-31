-- name: ListDecisionRequestCards :many
SELECT request.id AS request_id, message.id AS message_id, request.issue_id,
    COALESCE(workspace.issue_prefix, 'MUL')::text AS issue_prefix, issue.number AS issue_number,
    source.id AS source_comment_id, request.ask, request.status, request.created_at,
    request.withdraw_reason, request.cancel_reason, request.options,
    COALESCE(request.withdrawn_at, request.cancel_notice_posted_at) AS withdrawn_at,
    record.issue_comment_id AS record_comment_id,
    last_answer.id AS answer_id, last_answer.created_at AS answer_created_at, last_answer.woken_at AS answer_woken_at,
    last_answer.wake_outcome AS answer_wake_outcome, last_answer.disclaimed_at AS answer_disclaimed_at,
    last_answer.chosen_option_index AS answer_chosen_option_index,
    pickup.id AS pickup_task_id, COALESCE(pickup.agent_name, '')::text AS pickup_agent_name
FROM decision_request request
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
JOIN workspace ON workspace.id = request.workspace_id
JOIN chat_message message ON message.chat_session_id = request.chat_session_id
    AND (message.id = request.ping_message_id OR request.id = ANY(message.decision_request_ids))
LEFT JOIN comment source ON source.id = request.source_comment_id
    AND source.workspace_id = request.workspace_id AND source.issue_id = request.issue_id AND source.deleted_at IS NULL
LEFT JOIN decision_answer record ON record.decision_request_id = request.id AND request.status = 'answered'
    AND record.disclaimed_at IS NULL
LEFT JOIN LATERAL (
    SELECT answer.id, answer.created_at, answer.woken_at, answer.wake_outcome, answer.disclaimed_at, answer.chosen_option_index, answer.issue_comment_id
    FROM decision_answer answer WHERE answer.decision_request_id = request.id
    ORDER BY answer.created_at DESC, answer.id DESC LIMIT 1
) last_answer ON true
-- The run that resumed the issue: one that started and carries the record, else the first started run on the issue after it.
LEFT JOIN LATERAL (
    SELECT task.id, agent.name AS agent_name
    FROM agent_task_queue task
    JOIN agent ON agent.id = task.agent_id AND agent.workspace_id = request.workspace_id
    WHERE task.issue_id = request.issue_id AND last_answer.id IS NOT NULL AND task.started_at IS NOT NULL
        AND (task.trigger_comment_id = last_answer.issue_comment_id
            OR last_answer.issue_comment_id = ANY(task.coalesced_comment_ids)
            OR task.created_at >= last_answer.created_at)
    ORDER BY (task.trigger_comment_id = last_answer.issue_comment_id OR last_answer.issue_comment_id = ANY(task.coalesced_comment_ids)) DESC NULLS LAST,
        task.created_at, task.id
    LIMIT 1
) pickup ON true
WHERE request.authority = 'human' AND request.workspace_id = sqlc.arg('workspace_id')
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
WHERE request.authority = 'human' AND request.workspace_id = sqlc.arg('workspace_id')
    AND request.human_user_id = sqlc.arg('human_user_id')
    AND request.status = 'open'
ORDER BY request.created_at, request.id;

-- name: ListOpenDecisionAsksForHuman :many
-- Read-only projection for the open-asks read: first line of the ask and the
-- default text only, never options, answers, or the full ask.
SELECT request.id,
    (COALESCE(NULLIF(workspace.issue_prefix, ''), 'MUL') || '-' || issue.number::text)::text AS issue_identifier,
    request.issue_id, workspace.slug AS workspace_slug,
    COALESCE(NULLIF(split_part(btrim(request.ask_json->>'decision', E' \t\r\n'), E'\n', 1), ''), NULLIF(split_part(split_part(request.ask, E'**Decision**\n', 2), E'\n', 1), ''), split_part(request.ask, E'\n', 1))::text AS ask_headline,
    request.escalation_class, request.created_at, request.chat_session_id,
    request.asking_agent_id, COALESCE(request.ask_json->>'default_if_no_reply', '')::text AS default_if_no_reply
FROM decision_request request
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
JOIN workspace ON workspace.id = request.workspace_id
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
