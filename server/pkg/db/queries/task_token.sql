-- name: CreateTaskToken :one
INSERT INTO task_token (token_hash, task_id, agent_id, workspace_id, user_id, expires_at, id)
VALUES ($1, $2, $3, $4, $5, $6, COALESCE(sqlc.narg('id')::uuid, gen_random_uuid()))
RETURNING *;

-- name: CreateGovernanceProposalTaskToken :one
INSERT INTO task_token (
    token_hash, task_id, agent_id, workspace_id, user_id, expires_at,
    purpose, governance_case_id, governance_attempt_id,
    governance_attempt_fence, governance_evidence_epoch
)
SELECT sqlc.arg('token_hash')::text, attempt.task_id, attempt.candidate_id,
       case_record.workspace_id, sqlc.arg('user_id')::uuid, sqlc.arg('expires_at')::timestamptz,
       'governance_proposal', case_record.id, attempt.id, attempt.attempt_fence,
       case_record.evidence_epoch
FROM governance_attempt AS attempt
JOIN governance_case AS case_record
  ON case_record.workspace_id = attempt.workspace_id
 AND case_record.id = attempt.case_id
JOIN governance_evaluation AS evaluation
  ON evaluation.workspace_id = case_record.workspace_id
 AND evaluation.case_id = case_record.id
 AND evaluation.attempt_id = attempt.id
JOIN governance_workspace_config AS control
  ON control.workspace_id = case_record.workspace_id
WHERE case_record.workspace_id = sqlc.arg('workspace_id')::uuid
  AND case_record.id = sqlc.arg('case_id')::uuid
  AND attempt.id = sqlc.arg('attempt_id')::uuid
  AND attempt.task_id = sqlc.arg('task_id')::uuid
  AND attempt.candidate_id = sqlc.arg('agent_id')::uuid
  AND attempt.kind = 'agent'
  AND attempt.redacted_at IS NULL
  AND attempt.terminal_at IS NULL
  AND attempt.deadline_at > now()
  AND case_record.current_attempt_id = attempt.id
  AND case_record.state = 'agent_attempt'
  AND case_record.control_epoch = control.control_epoch
  AND evaluation.redacted_at IS NULL
  AND control.settings->>'jev_governance_enabled' = 'true'
RETURNING *;

-- name: GetTaskTokenByHash :one
SELECT * FROM task_token
WHERE token_hash = $1 AND expires_at > now();

-- name: LockTaskTokenByHash :one
SELECT * FROM task_token
WHERE token_hash = $1 AND expires_at > now()
FOR UPDATE;

-- name: DeleteTaskTokensByTask :exec
DELETE FROM task_token WHERE task_id = $1;

-- name: DeleteExpiredTaskTokens :exec
DELETE FROM task_token WHERE expires_at <= now();
