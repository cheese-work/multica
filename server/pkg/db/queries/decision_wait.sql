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

-- name: SuspendDecisionWait :one
-- The suspension write of a refusal (design §4.4.7): kind, clock, conflicting
-- task and the restore target the capture rule chose. The caller runs it last,
-- after the block write, so the status hook's flag change cannot override it.
UPDATE decision_wait SET
    phase = 'suspended', suspend_reason = sqlc.arg('suspend_reason'), suspended_at = now(),
    refused_task_id = sqlc.narg('refused_task_id'), dispatch_refused_since = NULL,
    restore_status = sqlc.arg('restore_status'), restore_assignee_type = sqlc.narg('restore_assignee_type'),
    restore_assignee_id = sqlc.narg('restore_assignee_id'), restore_on_resume = sqlc.arg('restore_on_resume')
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

-- name: GetBindingDecisionWait :one
-- The non-closed episode that binds a request (CHE-2591 L5 design §4.4.1).
SELECT * FROM decision_wait
WHERE workspace_id = sqlc.arg('workspace_id') AND closed_at IS NULL AND sqlc.arg('request_id')::uuid = ANY(request_ids);

-- name: GetDecisionWait :one
SELECT * FROM decision_wait WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id');

-- name: LockDecisionWait :one
SELECT * FROM decision_wait WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id') FOR UPDATE;

-- name: ListDecisionWaitRequests :many
SELECT * FROM decision_request
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND id = ANY(sqlc.arg('request_ids')::uuid[])
ORDER BY id;

-- name: ListDecisionWaitAnswers :many
SELECT answer.* FROM decision_answer answer
JOIN decision_request request ON request.authority = 'human' AND request.id = answer.decision_request_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.id = ANY(sqlc.arg('request_ids')::uuid[])
ORDER BY answer.id;

-- name: ListDecisionWaitIntents :many
SELECT * FROM decision_intent
WHERE workspace_id = sqlc.arg('workspace_id') AND id = ANY(sqlc.arg('intent_ids')::uuid[])
ORDER BY id;

-- name: ClaimDecisionWait :execrows
UPDATE decision_wait SET closed_at = now(), close_reason = sqlc.arg('close_reason')
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id') AND closed_at IS NULL;

-- name: RecordDecisionWaitResume :execrows
UPDATE decision_wait SET resume_task_id = sqlc.arg('resume_task_id'), resume_outcome = sqlc.arg('resume_outcome')
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id') AND close_reason IS NOT NULL;

-- name: MarkDecisionWaitDispatchRefused :exec
UPDATE decision_wait SET dispatch_refused_since = now()
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
  AND dispatch_refused_since IS NULL AND closed_at IS NULL AND phase = 'waiting';

-- name: MarkDecisionWaitCloseRefused :exec
-- A system close that cannot complete (no blocked step for no_resume_agent)
-- demotes the episode in any phase, so suspended ones rotate out of the batch too.
UPDATE decision_wait SET dispatch_refused_since = now()
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
  AND dispatch_refused_since IS NULL AND closed_at IS NULL;

-- name: ListDecisionEpisodeCandidates :many
-- Non-closed episodes that §6 can act on now (design §4.4.1). A closed issue
-- or an unavailable asking agent makes the episode a system-close candidate
-- (clause 1) whatever its requests say. Otherwise it is a candidate when it
-- can deliver (clauses 2-4): an episode that cannot progress must not take a
-- batch slot.
-- Episodes with a refused dispatch sort last, in an order that rotates with
-- the rotation argument, so a batch of persistent refusals cannot hold every
-- slot while a later one could now dispatch. The run-budget resume condition
-- is the member status change after the suspension or a later workflow budget
-- start; the delivery transaction re-checks it exactly.
SELECT wait.* FROM decision_wait wait
JOIN issue ON issue.id = wait.issue_id AND issue.workspace_id = wait.workspace_id
JOIN agent asker ON asker.id = wait.asking_agent_id AND asker.workspace_id = wait.workspace_id
LEFT JOIN project ON project.id = issue.project_id AND project.workspace_id = issue.workspace_id
WHERE wait.closed_at IS NULL
  AND (
    issue.status IN ('done', 'cancelled', 'agent_accepted', 'approved')
    OR asker.archived_at IS NOT NULL OR asker.runtime_id IS NULL OR asker.kind = 'system'
    OR (
  (
        (wait.phase = 'waiting' OR wait.suspend_reason = 'status_moved'
         OR (wait.suspend_reason = 'run_budget' AND (
             EXISTS (
                 SELECT 1 FROM activity_log moved
                 WHERE moved.issue_id = issue.id AND moved.action IN ('status_changed', 'run_budget_top_up')
                   AND moved.actor_type = 'member' AND moved.created_at > wait.suspended_at
                   AND moved.details->'trigger'->'cause'->>'kind' IS DISTINCT FROM 'workflow_remap'
                   AND (
                       moved.action = 'run_budget_top_up'
                       OR moved.created_at < (SELECT cutover_at FROM run_budget_window_cutover)
                       OR (moved.details->'trigger'->>'auth_source' = 'interactive_jwt' AND moved.details->'trigger'->'cause'->>'kind' = 'ui_or_api')
                   )
             ) OR EXISTS (
                 SELECT 1 FROM issue_workflow flow
                 WHERE flow.workspace_id = issue.workspace_id AND flow.run_budget_since > wait.suspended_at
                   AND (flow.id = issue.workflow_id OR (issue.workflow_id IS NULL AND (
                       flow.id = project.workflow_id OR (flow.is_default AND project.workflow_id IS NULL)
                   )))
             )
         ))
         OR (wait.suspend_reason = 'dispatch_refused' AND (
             wait.refused_task_id IS NULL
             OR NOT EXISTS (
                 SELECT 1 FROM agent_task_queue task
                 WHERE task.id = wait.refused_task_id AND task.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
             ) OR EXISTS (
                 SELECT 1 FROM activity_log moved
                 WHERE moved.issue_id = issue.id AND moved.action IN ('status_changed', 'run_budget_top_up')
                   AND moved.actor_type = 'member' AND moved.created_at > wait.suspended_at
                   AND moved.details->'trigger'->'cause'->>'kind' IS DISTINCT FROM 'workflow_remap'
             )
         )))
  )
  AND NOT EXISTS (
      SELECT 1 FROM decision_request bound
      WHERE bound.authority = 'human' AND bound.id = ANY(wait.request_ids) AND bound.status = 'open'
  )
  AND NOT EXISTS (
      SELECT 1 FROM decision_answer live
      JOIN decision_request bound ON bound.authority = 'human' AND bound.id = live.decision_request_id
      WHERE bound.id = ANY(wait.request_ids) AND bound.status = 'answered'
        AND live.disclaimed_at IS NULL AND live.woken_at IS NULL AND (live.wake_at IS NULL OR live.wake_at > clock_timestamp())
  )
    )
  )
ORDER BY (wait.dispatch_refused_since IS NOT NULL),
  CASE WHEN wait.dispatch_refused_since IS NOT NULL THEN md5(wait.id::text || sqlc.arg('rotation')::bigint::text) END,
  wait.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: ListDecisionReminderCandidates :many
-- Waiting episodes whose one reminder is due (design §4.6). The filters match
-- what the delivery transaction re-checks, so an episode that cannot be
-- reminded never takes a batch slot.
SELECT wait.* FROM decision_wait wait
WHERE wait.closed_at IS NULL AND wait.phase = 'waiting'
  AND wait.reminder_delivered_at IS NULL AND wait.reminder_due_at <= now()
  AND EXISTS (
      SELECT 1 FROM decision_request bound
      WHERE bound.authority = 'human' AND bound.id = ANY(wait.request_ids) AND bound.status = 'open'
        AND bound.human_user_id = wait.human_user_id
  )
  AND EXISTS (
      SELECT 1 FROM member WHERE member.workspace_id = wait.workspace_id AND member.user_id = wait.human_user_id
  )
ORDER BY wait.reminder_due_at, wait.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: MarkDecisionReminderDelivered :execrows
UPDATE decision_wait SET reminder_delivered_at = now()
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
  AND closed_at IS NULL AND reminder_delivered_at IS NULL;

-- name: ListDecisionWaitExpiredActionRequests :many
-- Bound requests whose prepared action expired: the episode closes
-- action_expired, not expired (design §6.1 priority 3).
SELECT action.decision_request_id FROM decision_action action
WHERE action.workspace_id = sqlc.arg('workspace_id') AND action.state = 'expired'
  AND action.decision_request_id = ANY(sqlc.arg('request_ids')::uuid[]);
