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

-- The approve and reject transaction takes every row lock NOWAIT (plan 3.2):
-- it is never a waiter in a lock cycle. Each row is locked once, at the
-- strongest mode a later statement needs.

-- name: LockWorkspaceKeyShareNowait :one
SELECT id FROM workspace WHERE id = $1 FOR KEY SHARE NOWAIT;

-- name: LockDecisionSessionNowait :one
SELECT id FROM chat_session
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE NOWAIT;

-- name: LockDecisionRequestNowait :one
SELECT * FROM decision_request
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR UPDATE NOWAIT;

-- name: LockDecisionLiveAnswerNowait :one
SELECT answer.* FROM decision_answer answer
JOIN decision_request request ON request.id = answer.decision_request_id
WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.id = sqlc.arg('id')
  AND answer.disclaimed_at IS NULL
FOR UPDATE OF answer NOWAIT;

-- name: LockDecisionActionNowait :one
SELECT * FROM decision_action WHERE id = $1 AND workspace_id = $2 FOR UPDATE NOWAIT;

-- name: LockDecisionActionMemberNowait :one
SELECT id FROM member
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
