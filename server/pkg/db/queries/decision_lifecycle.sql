-- name: ListDecisionLifecycleCancellations :many
SELECT request.* FROM decision_request request
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
LEFT JOIN agent ON agent.id = request.asking_agent_id AND agent.workspace_id = request.workspace_id
LEFT JOIN squad ON issue.assignee_type = 'squad' AND squad.id = issue.assignee_id AND squad.workspace_id = issue.workspace_id AND squad.archived_at IS NULL
LEFT JOIN agent fallback ON fallback.workspace_id = issue.workspace_id AND fallback.id = CASE
    WHEN issue.assignee_type = 'agent' THEN issue.assignee_id WHEN issue.assignee_type = 'squad' THEN squad.leader_id END
WHERE request.status IN ('open', 'answered') AND (
    issue.status IN ('done', 'cancelled', 'agent_accepted', 'approved')
    OR (agent.archived_at IS NOT NULL AND (request.status = 'open' OR fallback.id IS NULL OR fallback.archived_at IS NOT NULL OR fallback.runtime_id IS NULL))
    OR NOT EXISTS (SELECT 1 FROM member WHERE member.workspace_id = request.workspace_id AND member.user_id = request.human_user_id)
    OR (request.status = 'open' AND request.origin = 'asked' AND request.created_at <= CURRENT_TIMESTAMP - interval '168 hours')
    OR (request.status = 'open' AND EXISTS (
        SELECT 1 FROM decision_action action
        WHERE action.decision_request_id = request.id AND action.state = 'pending' AND action.expires_at <= clock_timestamp()
    ))
)
ORDER BY request.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: DecisionLifecycleReason :one
SELECT CASE
    WHEN issue.status IN ('done', 'cancelled', 'agent_accepted', 'approved') THEN 'issue_closed'
    WHEN NOT EXISTS (SELECT 1 FROM member WHERE member.workspace_id = request.workspace_id AND member.user_id = request.human_user_id) THEN 'human_removed'
    WHEN agent.archived_at IS NOT NULL AND (request.status = 'open' OR fallback.id IS NULL OR fallback.archived_at IS NOT NULL OR fallback.runtime_id IS NULL) THEN 'agent_archived'
    WHEN request.status = 'open' AND request.origin = 'asked' AND request.created_at <= CURRENT_TIMESTAMP - interval '168 hours' THEN 'expired'
    WHEN request.status = 'open' AND EXISTS (
        SELECT 1 FROM decision_action action
        WHERE action.decision_request_id = request.id AND action.state = 'pending' AND action.expires_at <= clock_timestamp()
    ) THEN 'expired'
    ELSE ''
END::text AS reason
FROM decision_request request
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
LEFT JOIN agent ON agent.id = request.asking_agent_id AND agent.workspace_id = request.workspace_id
LEFT JOIN squad ON issue.assignee_type = 'squad' AND squad.id = issue.assignee_id AND squad.workspace_id = issue.workspace_id AND squad.archived_at IS NULL
LEFT JOIN agent fallback ON fallback.workspace_id = issue.workspace_id AND fallback.id = CASE
    WHEN issue.assignee_type = 'agent' THEN issue.assignee_id WHEN issue.assignee_type = 'squad' THEN squad.leader_id END
WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.id = sqlc.arg('id');

-- name: CancelDecisionLifecycleRequest :one
UPDATE decision_request SET status = 'cancelled', cancel_reason = sqlc.arg('reason'), cancel_notice_posted_at = clock_timestamp()
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id') AND status IN ('open', 'answered')
RETURNING *;

-- name: ListDecisionRetractionCandidates :many
SELECT answer.*, request.workspace_id FROM decision_answer answer
JOIN decision_request request ON request.id = answer.decision_request_id
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
WHERE answer.woken_at IS NOT NULL AND answer.wake_outcome <> 'skipped_ineligible'
  AND answer.disclaimed_at IS NOT NULL AND answer.withdrawal_comment_id IS NOT NULL
  AND answer.retraction_delivered_at IS NULL
ORDER BY answer.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: ListDecisionReaskCandidates :many
SELECT request.* FROM decision_request request
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
WHERE request.status = 'cancelled' AND request.cancel_reason IN ('human_removed', 'lane_archived')
  AND request.cancel_notice_posted_at IS NOT NULL AND request.reask_delivered_at IS NULL
ORDER BY request.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: GetDecisionCancelComment :one
SELECT * FROM comment
WHERE workspace_id = sqlc.arg('workspace_id') AND decision_request_id = sqlc.arg('id') AND decision_comment_kind = 'cancel';

-- name: AcknowledgeDecisionRetraction :execrows
UPDATE decision_answer answer SET retraction_delivered_at = clock_timestamp(), retraction_outcome = sqlc.arg('outcome')
FROM decision_request request
WHERE request.id = answer.decision_request_id AND request.workspace_id = sqlc.arg('workspace_id')
  AND answer.id = sqlc.arg('id') AND answer.retraction_delivered_at IS NULL
  AND answer.disclaimed_at IS NOT NULL AND answer.withdrawal_comment_id IS NOT NULL
  AND answer.woken_at IS NOT NULL AND answer.wake_outcome <> 'skipped_ineligible';

-- name: AcknowledgeDecisionReask :execrows
UPDATE decision_request SET reask_delivered_at = clock_timestamp(), reask_outcome = sqlc.arg('outcome')
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id') AND reask_delivered_at IS NULL
  AND status = 'cancelled' AND cancel_reason IN ('human_removed', 'lane_archived') AND cancel_notice_posted_at IS NOT NULL;

-- name: ListDecisionCancelSiblings :many
SELECT * FROM decision_request
WHERE workspace_id = sqlc.arg('workspace_id') AND issue_id = sqlc.arg('issue_id') AND chat_session_id = sqlc.arg('chat_session_id')
  AND status IN ('open', 'answered')
ORDER BY created_at, id;

-- name: DecisionIssueClosedAt :one
-- Comments bump issue.updated_at, so the close time comes from the status change record;
-- updated_at only covers an issue whose record is missing or not yet written.
SELECT COALESCE(
    (SELECT max(activity.created_at) FROM activity_log activity
     WHERE activity.workspace_id = issue.workspace_id AND activity.issue_id = issue.id
       AND activity.action = 'status_changed' AND activity.details->>'to' = issue.status),
    issue.updated_at
)::timestamptz AS closed_at
FROM issue
WHERE issue.workspace_id = sqlc.arg('workspace_id') AND issue.id = sqlc.arg('id');

-- name: HasDecisionCancelNoticeSinceClose :one
SELECT EXISTS (
    SELECT 1 FROM decision_request request
    WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.issue_id = sqlc.arg('issue_id')
      AND request.chat_session_id = sqlc.arg('chat_session_id') AND request.id <> sqlc.arg('id')
      AND request.status = 'cancelled' AND request.cancel_reason = 'issue_closed'
      AND request.cancel_notice_posted_at >= sqlc.arg('closed_at')::timestamptz
)::bool AS posted;
