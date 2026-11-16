-- name: UpsertIssueStepAssignee :exec
INSERT INTO issue_step_assignee (issue_id, status_key, workspace_id, assignee_type, assignee_id, set_by_type, set_by_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (issue_id, status_key) DO UPDATE SET
    assignee_type = EXCLUDED.assignee_type,
    assignee_id = EXCLUDED.assignee_id,
    set_by_type = EXCLUDED.set_by_type,
    set_by_id = EXCLUDED.set_by_id,
    updated_at = now();

-- name: GetIssueStepAssignee :one
SELECT * FROM issue_step_assignee
WHERE issue_id = $1 AND status_key = $2 AND workspace_id = $3;

-- name: DeleteIssueStepAssignee :execrows
DELETE FROM issue_step_assignee
WHERE issue_id = $1 AND status_key = $2 AND workspace_id = $3;
