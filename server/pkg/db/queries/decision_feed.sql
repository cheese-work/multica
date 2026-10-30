-- name: ListDecisionAnswerFeed :many
SELECT answer.id, answer.state_version, answer.effective_at, answer.wake_at, answer.woken_at, answer.wake_outcome,
       answer.disclaimed_at, answer.disclaim_reason, request.id AS decision_request_id, request.ask, request.escalation_class,
       issue.id AS issue_id, issue.number AS issue_number, workspace.issue_prefix, project.title AS project_title,
       quote.content AS quote
FROM decision_answer answer
JOIN decision_request request ON request.id = answer.decision_request_id
JOIN issue ON issue.id = request.issue_id
JOIN workspace ON workspace.id = request.workspace_id
LEFT JOIN project ON project.id = issue.project_id
JOIN comment quote ON quote.decision_answer_id = answer.id AND quote.decision_comment_kind = 'quote'
WHERE request.workspace_id = sqlc.arg('workspace_id')
  AND answer.state_version > answer.hindsight_acked_version
ORDER BY answer.created_at, answer.id
LIMIT sqlc.arg('row_limit');

-- name: AckDecisionAnswerFeed :one
UPDATE decision_answer answer SET hindsight_acked_version = sqlc.arg('version')
FROM decision_request request
WHERE request.id = answer.decision_request_id AND request.workspace_id = sqlc.arg('workspace_id')
  AND answer.id = sqlc.arg('id') AND answer.hindsight_acked_version < sqlc.arg('version')
  AND answer.state_version >= sqlc.arg('version')
RETURNING answer.id, answer.hindsight_acked_version;
