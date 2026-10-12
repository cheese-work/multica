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

-- name: StampCommentDecisionIntent :exec
UPDATE comment SET decision_intent_id = sqlc.arg('decision_intent_id')
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('comment_id');

-- name: CloseDecisionIntent :exec
UPDATE decision_intent
SET closed_at = COALESCE(closed_at, clock_timestamp()), close_reason = sqlc.arg('close_reason'),
    sealed_at = COALESCE(sealed_at, clock_timestamp())
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id');

-- name: SealDecisionIntent :exec
UPDATE decision_intent SET sealed_at = COALESCE(sealed_at, clock_timestamp())
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id');

-- name: DecisionIntentRefusal :one
SELECT CASE
    WHEN intent.sealed_at IS NOT NULL THEN 'sealed'
    WHEN intent.closed_at IS NOT NULL THEN 'closed'
    WHEN tag.id IS NULL OR tag.deleted_at IS NOT NULL THEN 'comment_deleted'
    WHEN task.id IS NULL OR task.status <> 'running' THEN 'task_stopped'
    WHEN clock_timestamp() >= intent.expires_at THEN 'expired'
    WHEN intent.consumed_at IS NOT NULL OR NOT EXISTS (
        SELECT 1 FROM decision_intent_target target
        WHERE target.decision_intent_id = intent.id AND target.consumed_at IS NULL
    ) THEN 'consumed'
    ELSE ''
END::text AS reason
FROM decision_intent intent
LEFT JOIN comment tag ON tag.id = intent.comment_id AND tag.issue_id = intent.issue_id AND tag.workspace_id = intent.workspace_id
LEFT JOIN agent_task_queue task ON task.id = intent.task_id
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('id');

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

-- name: ListDecisionPingRetries :many
SELECT * FROM decision_request
WHERE origin = 'asked' AND ping_notified_at IS NULL
ORDER BY created_at, id
LIMIT $1;

-- name: MarkDecisionPingNotified :one
UPDATE decision_request
SET ping_notified_at = clock_timestamp()
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
  AND ping_notified_at IS NULL
RETURNING *;

-- name: ConsumeDecisionIntentTarget :execrows
UPDATE decision_intent_target target
SET consumed_at = clock_timestamp(), consumed_by_request_id = sqlc.arg('request_id')
FROM decision_intent intent
WHERE intent.id = target.decision_intent_id AND intent.workspace_id = sqlc.arg('workspace_id')
  AND intent.id = sqlc.arg('decision_intent_id') AND target.human_user_id = sqlc.arg('human_user_id')
  AND target.consumed_at IS NULL;

-- name: CompleteDecisionIntent :exec
UPDATE decision_intent intent
SET consumed_at = clock_timestamp(), consumed_by_request_id = sqlc.arg('request_id'),
    sealed_at = clock_timestamp()
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('id')
  AND intent.consumed_at IS NULL AND intent.sealed_at IS NULL AND intent.closed_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM decision_intent_target target
      WHERE target.decision_intent_id = intent.id AND target.consumed_at IS NULL
  );

-- name: GetDecisionIntentForTask :one
SELECT * FROM decision_intent
WHERE workspace_id = sqlc.arg('workspace_id') AND issue_id = sqlc.arg('issue_id')
  AND task_id = sqlc.arg('task_id') AND agent_id = sqlc.arg('agent_id')
ORDER BY opened_at DESC, id DESC
LIMIT 1;

-- name: ListDecisionIntentTargets :many
SELECT target.* FROM decision_intent_target target
JOIN decision_intent intent ON intent.id = target.decision_intent_id
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('decision_intent_id')
ORDER BY target.human_user_id;

-- name: ListDecisionLanes :many
SELECT session.id, session.title, session.lane_summary, session.lane_summary_updated_at, session.updated_at
FROM chat_session session
WHERE session.workspace_id = sqlc.arg('workspace_id') AND session.agent_id = sqlc.arg('agent_id')
  AND session.creator_id = sqlc.arg('creator_id') AND session.status = 'active'
  AND session.channel_origin_at IS NULL AND NOT session.is_agent_intro
  AND NOT EXISTS (SELECT 1 FROM channel_chat_session_binding binding WHERE binding.chat_session_id = session.id)
ORDER BY session.updated_at DESC, session.id DESC;

-- name: DecisionLaneWindowOpen :one
SELECT EXISTS (
    SELECT 1 FROM decision_intent intent
    JOIN decision_intent_target target ON target.decision_intent_id = intent.id
    JOIN comment tag ON tag.id = intent.comment_id AND tag.issue_id = intent.issue_id AND tag.workspace_id = intent.workspace_id
    JOIN agent_task_queue task ON task.id = intent.task_id AND task.agent_id = intent.agent_id AND task.issue_id = intent.issue_id
    JOIN agent asking ON asking.id = intent.agent_id AND asking.workspace_id = intent.workspace_id
    JOIN member human ON human.user_id = target.human_user_id AND human.workspace_id = intent.workspace_id
    WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('decision_intent_id')
      AND target.human_user_id = sqlc.arg('human_user_id') AND target.consumed_at IS NULL
      AND intent.sealed_at IS NULL AND intent.closed_at IS NULL AND intent.consumed_at IS NULL
      AND tag.deleted_at IS NULL AND task.status = 'running' AND task.chat_session_id IS NULL
      AND asking.archived_at IS NULL AND clock_timestamp() < intent.expires_at
)::boolean AS open;

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
