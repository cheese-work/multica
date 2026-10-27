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
