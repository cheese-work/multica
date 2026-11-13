-- name: CreateDecisionAction :one
INSERT INTO decision_action (
    workspace_id, decision_request_id, action_type, action_version,
    params, preconditions, secret_slots, payload_sha256, expires_at,
    requested_by_agent_id, requested_by_task_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, sqlc.narg('requested_by_task_id')
)
RETURNING *;

-- name: GetDecisionAction :one
SELECT * FROM decision_action WHERE id = $1 AND workspace_id = $2;

-- name: GetDecisionActionByRequest :one
SELECT * FROM decision_action WHERE decision_request_id = $1 AND workspace_id = $2;

-- A batch's actions in id order; the first is the batch's lead, the action its
-- passkey challenge names.
-- name: ListDecisionBatchActions :many
SELECT action.* FROM decision_action action
JOIN decision_request request ON request.id = action.decision_request_id AND request.workspace_id = action.workspace_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.batch_id = sqlc.arg('batch_id')
ORDER BY action.id;

-- The approve and reject transaction takes every row lock NOWAIT (plan 3.2):
-- it is never a waiter in a lock cycle. Each row is locked once, at the
-- strongest mode a later statement needs.

-- name: LockWorkspaceKeyShareNowait :one
SELECT id FROM workspace WHERE id = $1 FOR KEY SHARE NOWAIT;

-- A config change serialises per field and target on a try-lock: a second
-- approval of the same field gets false and retries, never waits.
-- name: TryLockConfigChange :one
SELECT pg_try_advisory_xact_lock(hashtextextended(sqlc.arg('key')::text, 0))::boolean;

-- The row a config change reads and writes is locked the same way, so an
-- ordinary edit cannot commit between the approval's read and its write.
-- name: LockWorkflowTargetNowait :one
SELECT id FROM issue_workflow
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id')
FOR UPDATE NOWAIT;

-- name: LockDecisionSessionNowait :one
SELECT id FROM chat_session
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE NOWAIT;

-- name: LockDecisionRequestNowait :one
SELECT * FROM decision_request
WHERE authority = 'human' AND workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE NOWAIT;

-- name: LockDecisionLiveAnswerNowait :one
SELECT answer.* FROM decision_answer answer
JOIN decision_request request ON request.authority = 'human' AND request.id = answer.decision_request_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.id = sqlc.arg('id')
  AND answer.disclaimed_at IS NULL
FOR UPDATE OF answer NOWAIT;

-- name: LockDecisionActionNowait :one
SELECT * FROM decision_action WHERE id = $1 AND workspace_id = $2 FOR UPDATE NOWAIT;

-- name: LockDecisionActionMemberNowait :one
SELECT * FROM member
WHERE workspace_id = sqlc.arg('workspace_id') AND user_id = sqlc.arg('user_id')
FOR SHARE NOWAIT;

-- name: RejectDecisionActionRow :one
UPDATE decision_action SET state = 'rejected', updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND state = 'pending'
RETURNING *;

-- name: ReadDecisionActionForShare :one
-- The synchronized readback: blocks (under the caller's lock_timeout) until the
-- transaction that holds the row FOR UPDATE has committed or aborted.
SELECT * FROM decision_action WHERE id = $1 AND workspace_id = $2 FOR SHARE;

-- name: LockDecisionAgentsNowait :many
-- The asking agent and the action's target agent, each locked once, in id order.
SELECT * FROM agent
WHERE workspace_id = sqlc.arg('workspace_id') AND id = ANY(sqlc.arg('ids')::uuid[])
ORDER BY id
FOR UPDATE NOWAIT;

-- name: LockDecisionSquadNowait :one
-- The squad a squad-target action changes, locked in its workspace.
SELECT * FROM squad
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE NOWAIT;

-- name: LockDecisionWorkflowNowait :one
SELECT * FROM issue_workflow
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE NOWAIT;

-- name: LockDecisionWorkspaceNowait :one
-- A workspace-target action rewrites a column of the row row 1 holds FOR KEY
-- SHARE; take the stronger mode the update needs, once.
SELECT * FROM workspace WHERE id = $1 FOR NO KEY UPDATE NOWAIT;

-- name: LockDecisionIssueNowait :one
SELECT id FROM issue
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR NO KEY UPDATE NOWAIT;

-- name: TryLockDecisionCommentThread :one
-- Same advisory key as LockCommentThread, but never waits.
SELECT pg_try_advisory_xact_lock(hashtextextended(sqlc.arg(thread_root_id)::uuid::text || ':comment_thread', 0))::boolean;

-- name: FinishDecisionActionRow :one
-- Execution and the terminal state commit together; there is no "executing" row.
UPDATE decision_action SET
    state = sqlc.arg('state'), approved_by_user_id = sqlc.arg('approved_by_user_id'),
    approved_at = clock_timestamp(), executed_at = clock_timestamp(),
    webauthn_credential_id = sqlc.narg('webauthn_credential_id'), result = sqlc.narg('result'),
    error_code = sqlc.narg('error_code'), error_message = sqlc.narg('error_message'), updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id') AND state = 'pending'
  AND sqlc.arg('state') IN ('executed', 'failed')
RETURNING *;

-- name: MarkDecisionActionPostCommitDone :exec
UPDATE decision_action SET post_commit_done_at = clock_timestamp()
WHERE id = $1 AND workspace_id = $2 AND state IN ('executed', 'failed') AND post_commit_done_at IS NULL;

-- name: DecisionActionExpired :one
-- Judged on the database clock, never the application's.
SELECT expires_at <= clock_timestamp() FROM decision_action WHERE id = $1 AND workspace_id = $2;

-- name: ListDecisionActionsAwaitingPostCommit :many
-- The 30 s grace keeps the sweeper off an approval whose own request is still
-- doing its post-commit work.
SELECT * FROM decision_action
WHERE state IN ('executed', 'failed') AND post_commit_done_at IS NULL
  AND executed_at <= clock_timestamp() - interval '30 seconds'
ORDER BY executed_at, id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: LockAgentRuntimeNowait :one
-- Row 2 of the approve lock sequence (agent.copy only): the same mode as
-- LockAgentRuntimeForAccessChange, the runtime-access fence of agent create.
SELECT * FROM agent_runtime WHERE id = $1 AND workspace_id = $2 FOR NO KEY UPDATE NOWAIT;

-- name: LockPendingDecisionAction :one
-- Cancel, withdraw and expiry end the action after their own last lock. This
-- one waits: approve never does, so no cycle can form through it.
SELECT * FROM decision_action
WHERE workspace_id = $1 AND decision_request_id = $2 AND state = 'pending'
FOR UPDATE;

-- name: EndDecisionAction :one
UPDATE decision_action SET state = sqlc.arg('state'), updated_at = now()
WHERE id = sqlc.arg('id') AND workspace_id = sqlc.arg('workspace_id') AND state = 'pending'
  AND sqlc.arg('state') IN ('expired', 'cancelled')
RETURNING *;

-- name: ListRecentRejectedProposalParams :many
-- Params of the actions this person rejected within the cooldown, for one
-- agent, issue and action. The caller matches the params hash, which is
-- lane-independent: a rejection in any lane counts.
SELECT a.params, a.updated_at
FROM decision_action a
JOIN decision_request r ON r.id = a.decision_request_id AND r.workspace_id = a.workspace_id
WHERE a.workspace_id = sqlc.arg('workspace_id')
  AND a.state = 'rejected'
  AND a.requested_by_agent_id = sqlc.arg('agent_id')
  AND r.human_user_id = sqlc.arg('recipient_user_id')
  AND r.issue_id = sqlc.arg('issue_id')
  AND a.action_type = sqlc.arg('action_type')
  AND a.action_version = sqlc.arg('action_version')
  AND a.updated_at > now() - sqlc.arg('cooldown')::interval;

-- name: SetIssueProjectFromDecision :one
-- Moves an issue to the project an approved issue.project.set action names,
-- only while the issue is still in the project the card was asked from.
UPDATE issue SET project_id = sqlc.arg('project_id')::uuid, updated_at = now()
WHERE id = sqlc.arg('id')::uuid AND workspace_id = sqlc.arg('workspace_id')::uuid
  AND project_id IS NOT DISTINCT FROM sqlc.narg('from_project_id')::uuid
RETURNING *;
