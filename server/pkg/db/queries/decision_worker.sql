-- name: AcquireDecisionWorkerLease :one
INSERT INTO decision_worker_lease (name, holder, token, expires_at)
VALUES ('decision_consequences', sqlc.arg('holder'), sqlc.arg('token'), clock_timestamp() + interval '30 seconds')
ON CONFLICT (name) DO UPDATE
SET holder = EXCLUDED.holder, token = EXCLUDED.token, expires_at = clock_timestamp() + interval '30 seconds'
WHERE decision_worker_lease.expires_at <= clock_timestamp()
   OR decision_worker_lease.holder = ''
   OR (decision_worker_lease.holder = EXCLUDED.holder AND decision_worker_lease.token = EXCLUDED.token)
RETURNING *;

-- name: RenewDecisionWorkerLease :execrows
UPDATE decision_worker_lease
SET expires_at = clock_timestamp() + interval '30 seconds'
WHERE name = 'decision_consequences' AND holder = sqlc.arg('holder') AND token = sqlc.arg('token')
  AND expires_at > clock_timestamp();

-- name: ReleaseDecisionWorkerLease :exec
UPDATE decision_worker_lease SET expires_at = clock_timestamp()
WHERE name = 'decision_consequences' AND holder = sqlc.arg('holder') AND token = sqlc.arg('token');

-- name: ListDecisionFinalizations :many
SELECT request.* FROM decision_request request
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
JOIN chat_session session ON session.id = request.chat_session_id AND session.workspace_id = request.workspace_id
WHERE request.authority = 'human' AND EXISTS (
    SELECT 1 FROM decision_answer answer
    JOIN agent_task_queue task ON task.id = answer.answered_by_task_id
    WHERE answer.decision_request_id = request.id AND answer.disclaimed_at IS NULL AND answer.wake_at IS NULL
      AND (task.status IN ('failed', 'cancelled') OR (task.status = 'completed' AND request.status = 'answered'))
)
ORDER BY request.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: ScheduleDecisionAnswer :one
UPDATE decision_answer answer SET wake_at = GREATEST(answer.effective_at, clock_timestamp())
FROM decision_request request
WHERE request.authority = 'human' AND request.id = answer.decision_request_id AND request.workspace_id = sqlc.arg('workspace_id')
  AND request.status = 'answered' AND answer.id = sqlc.arg('id')
  AND answer.wake_at IS NULL AND answer.disclaimed_at IS NULL
RETURNING answer.*;

-- name: ListDecisionOrphanScopes :many
SELECT workspace_id, issue_id FROM (
    SELECT intent.workspace_id, intent.issue_id FROM decision_intent intent
    WHERE NOT EXISTS (SELECT 1 FROM workspace WHERE workspace.id = intent.workspace_id)
       OR NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = intent.issue_id AND issue.workspace_id = intent.workspace_id)
    UNION
    SELECT request.workspace_id, request.issue_id FROM decision_request request
    WHERE NOT EXISTS (SELECT 1 FROM workspace WHERE workspace.id = request.workspace_id)
       OR NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = request.issue_id AND issue.workspace_id = request.workspace_id)
) scopes
ORDER BY workspace_id, issue_id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: LockDecisionOrphanIntents :many
SELECT intent.id FROM decision_intent intent
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.issue_id = sqlc.arg('issue_id')
  AND (NOT EXISTS (SELECT 1 FROM workspace WHERE workspace.id = intent.workspace_id)
    OR NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = intent.issue_id AND issue.workspace_id = intent.workspace_id))
ORDER BY intent.id
LIMIT 100
FOR UPDATE;

-- name: DeleteDecisionOrphanIntent :exec
DELETE FROM decision_intent intent
WHERE intent.workspace_id = sqlc.arg('workspace_id') AND intent.id = sqlc.arg('id')
  AND (NOT EXISTS (SELECT 1 FROM workspace WHERE workspace.id = intent.workspace_id)
    OR NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = intent.issue_id AND issue.workspace_id = intent.workspace_id));

-- name: LockDecisionOrphanRequests :many
SELECT request.id FROM decision_request request
WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.issue_id = sqlc.arg('issue_id')
  AND (NOT EXISTS (SELECT 1 FROM workspace WHERE workspace.id = request.workspace_id)
    OR NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = request.issue_id AND issue.workspace_id = request.workspace_id))
ORDER BY request.id
LIMIT 100
FOR UPDATE;

-- name: DeleteDecisionOrphanRequest :exec
DELETE FROM decision_request request
WHERE request.workspace_id = sqlc.arg('workspace_id') AND request.id = sqlc.arg('id')
  AND (NOT EXISTS (SELECT 1 FROM workspace WHERE workspace.id = request.workspace_id)
    OR NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = request.issue_id AND issue.workspace_id = request.workspace_id));

-- name: DeleteDecisionOrphanRequestAction :exec
-- Called only for a request LockDecisionOrphanRequests returned, in the same
-- transaction, before DeleteDecisionOrphanRequest.
DELETE FROM decision_action
WHERE decision_request_id = $1;

-- name: DeleteDecisionOrphanAgentScope :exec
-- Authority-neutral orphan cleanup, before deleting the parent request.
DELETE FROM decision_agent_scope scope
WHERE scope.instance_id = sqlc.arg('id') AND scope.workspace_id = sqlc.arg('workspace_id')
  AND NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = scope.issue_id AND issue.workspace_id = scope.workspace_id);

-- name: DeleteDecisionOrphanAgentReliance :exec
DELETE FROM decision_agent_reliance reliance
WHERE reliance.instance_id = sqlc.arg('id') AND reliance.workspace_id = sqlc.arg('workspace_id')
  AND NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = reliance.issue_id AND issue.workspace_id = reliance.workspace_id);

-- name: DeleteDecisionOrphanAgentSettlement :exec
DELETE FROM decision_agent_settlement settlement
WHERE settlement.instance_id = sqlc.arg('id') AND settlement.workspace_id = sqlc.arg('workspace_id')
  AND NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = settlement.issue_id AND issue.workspace_id = settlement.workspace_id);

-- name: DeleteDecisionOrphanAgentUndo :exec
DELETE FROM decision_agent_undo undo
WHERE undo.instance_id = sqlc.arg('id') AND undo.workspace_id = sqlc.arg('workspace_id')
  AND NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = undo.issue_id AND issue.workspace_id = undo.workspace_id);
