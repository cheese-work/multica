-- name: ListDecisionVetoNotices :many
SELECT answer.*, request.workspace_id FROM decision_answer answer
JOIN decision_request request ON request.authority = 'human' AND request.id = answer.decision_request_id
JOIN issue ON issue.id = request.issue_id AND issue.workspace_id = request.workspace_id
WHERE answer.disclaimed_at IS NOT NULL AND answer.disclaim_reason IN ('agent_veto', 'record_retracted')
  AND answer.veto_note_posted_at IS NULL
ORDER BY answer.id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: MarkDecisionVetoNotified :exec
UPDATE decision_answer answer SET veto_note_posted_at = clock_timestamp()
FROM decision_request request
WHERE request.authority = 'human' AND request.id = answer.decision_request_id AND request.workspace_id = sqlc.arg('workspace_id')
  AND answer.id = sqlc.arg('id') AND answer.veto_note_posted_at IS NULL;
