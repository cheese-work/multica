-- name: InsertDecisionBoundWrite :execrows
INSERT INTO decision_bound_write (decision_id, workspace_id, agent_id, field, before_sha256, after_sha256)
VALUES (sqlc.arg('decision_id'), sqlc.arg('workspace_id'), sqlc.arg('agent_id'), sqlc.arg('field'), sqlc.arg('before_sha256'), sqlc.arg('after_sha256'))
ON CONFLICT (decision_id) DO NOTHING;

-- name: GetDecisionBindingAnswer :one
-- The live answer of an answered request, locked like LockDecisionLiveAnswer so
-- a disclaim or veto waits for the caller's transaction, with the human's role
-- and the database clock the expiry window is measured against.
SELECT answer.content_snapshot, answer.effective_at, answer.created_at,
       member.role AS human_role, clock_timestamp()::timestamptz AS now
FROM decision_request request
JOIN decision_answer answer ON answer.decision_request_id = request.id AND answer.disclaimed_at IS NULL
JOIN member ON member.workspace_id = request.workspace_id AND member.user_id = request.human_user_id
WHERE request.authority = 'human' AND request.workspace_id = sqlc.arg('workspace_id') AND request.id = sqlc.arg('id')
  AND request.status = 'answered'
FOR UPDATE OF answer;

-- name: DeleteDecisionOrphanBoundWrite :exec
-- The table has no foreign key; this runs before DeleteDecisionOrphanRequest
-- and removes the audit row under the same orphan condition.
DELETE FROM decision_bound_write bound
USING decision_request request
WHERE request.id = bound.decision_id AND request.workspace_id = sqlc.arg('workspace_id') AND request.id = sqlc.arg('id')
  AND (NOT EXISTS (SELECT 1 FROM workspace WHERE workspace.id = request.workspace_id)
    OR NOT EXISTS (SELECT 1 FROM issue WHERE issue.id = request.issue_id AND issue.workspace_id = request.workspace_id));
