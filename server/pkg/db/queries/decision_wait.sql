-- name: ListDecisionWaitBoundRequests :many
-- The bound set B of an entry (CHE-2591 L5 design §4.3 step 2): every open
-- asked request of the asking agent on the issue, and every request of each
-- intent those requests belong to.
SELECT request.* FROM decision_request request
WHERE request.authority = 'human' AND request.workspace_id = sqlc.arg('workspace_id') AND request.issue_id = sqlc.arg('issue_id')
  AND (
    (request.status = 'open' AND request.origin = 'asked' AND request.asking_agent_id = sqlc.arg('asking_agent_id'))
    OR request.decision_intent_id IN (
        SELECT mine.decision_intent_id FROM decision_request mine
        WHERE mine.authority = 'human' AND mine.workspace_id = sqlc.arg('workspace_id') AND mine.issue_id = sqlc.arg('issue_id')
          AND mine.status = 'open' AND mine.origin = 'asked' AND mine.asking_agent_id = sqlc.arg('asking_agent_id')
          AND mine.decision_intent_id IS NOT NULL
    )
  )
ORDER BY request.id;

-- name: ListDecisionWaitOpenAskers :many
-- Agents with an open asked request on the issue, for an entry no task makes.
SELECT DISTINCT asking_agent_id FROM decision_request
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND issue_id = sqlc.arg('issue_id')
  AND status = 'open' AND origin = 'asked'
ORDER BY asking_agent_id;

-- name: ListDecisionWaitPendingRetractions :many
-- Answers of the requests that match the retraction predicate of
-- ListDecisionRetractionCandidates.
SELECT answer.id FROM decision_answer answer
WHERE answer.decision_request_id = ANY(sqlc.arg('request_ids')::uuid[])
  AND answer.woken_at IS NOT NULL AND answer.wake_outcome <> 'skipped_ineligible'
  AND answer.disclaimed_at IS NOT NULL AND answer.withdrawal_comment_id IS NOT NULL
  AND answer.retraction_delivered_at IS NULL;

-- name: ListDecisionWaitLaneFailures :many
-- Requests whose DM lane cannot carry the ask: no ping message, a ping that
-- failed, or an archived lane.
SELECT request.id FROM decision_request request
LEFT JOIN chat_session lane ON lane.id = request.chat_session_id AND lane.workspace_id = request.workspace_id
LEFT JOIN chat_message ping ON ping.id = request.ping_message_id AND ping.chat_session_id = request.chat_session_id
WHERE request.authority = 'human' AND request.workspace_id = sqlc.arg('workspace_id') AND request.id = ANY(sqlc.arg('request_ids')::uuid[])
  AND (lane.id IS NULL OR lane.status <> 'active' OR ping.id IS NULL OR COALESCE(ping.failure_reason, '') <> '')
ORDER BY request.id;

-- name: GetOpenDecisionWait :one
SELECT * FROM decision_wait
WHERE workspace_id = sqlc.arg('workspace_id') AND issue_id = sqlc.arg('issue_id') AND closed_at IS NULL
FOR UPDATE;

-- name: CreateDecisionWait :one
INSERT INTO decision_wait (
    workspace_id, issue_id, asking_agent_id, asking_task_id, resume_squad_id, human_user_id,
    request_ids, intent_ids, group_ack_intent_ids,
    restore_status, restore_assignee_type, restore_assignee_id, restore_on_resume, reminder_due_at
)
VALUES (
    sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('asking_agent_id'), sqlc.narg('asking_task_id'), sqlc.narg('resume_squad_id'), sqlc.arg('human_user_id'),
    sqlc.arg('request_ids')::uuid[], sqlc.arg('intent_ids')::uuid[], sqlc.arg('group_ack_intent_ids')::uuid[],
    sqlc.arg('restore_status'), sqlc.narg('restore_assignee_type'), sqlc.narg('restore_assignee_id'), true,
    GREATEST(
        now(),
        LEAST(
            now() + interval '24 hours',
            COALESCE((
                SELECT min(action.expires_at) - interval '2 hours' FROM decision_action action
                WHERE action.state = 'pending' AND action.decision_request_id = ANY(sqlc.arg('request_ids')::uuid[])
            ), 'infinity'::timestamptz)
        )
    )
)
RETURNING *;

-- name: ReattachDecisionWait :one
-- Phase waiting again, with the human and the restore target the capture rule
-- chose. B, the acknowledged intents and the squad context do not change.
UPDATE decision_wait SET
    phase = 'waiting', suspend_reason = NULL, suspended_at = NULL, refused_task_id = NULL, dispatch_refused_since = NULL,
    human_user_id = sqlc.arg('human_user_id'),
    restore_status = sqlc.arg('restore_status'), restore_assignee_type = sqlc.narg('restore_assignee_type'),
    restore_assignee_id = sqlc.narg('restore_assignee_id'), restore_on_resume = true
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id') AND closed_at IS NULL
RETURNING *;

-- name: MoveDecisionWaitStatus :one
-- A non-episode status write: the restore flag goes false. In phase waiting,
-- a new status that is not closed also suspends the episode as status_moved.
UPDATE decision_wait SET
    restore_on_resume = false,
    phase = CASE WHEN sqlc.arg('suspend')::boolean AND phase = 'waiting' THEN 'suspended' ELSE phase END,
    suspend_reason = CASE WHEN sqlc.arg('suspend')::boolean AND phase = 'waiting' THEN 'status_moved' ELSE suspend_reason END,
    suspended_at = CASE WHEN sqlc.arg('suspend')::boolean AND phase = 'waiting' THEN now() ELSE suspended_at END,
    refused_task_id = CASE WHEN sqlc.arg('suspend')::boolean AND phase = 'waiting' THEN NULL ELSE refused_task_id END
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id') AND closed_at IS NULL
RETURNING *;
