-- name: InsertIssueDecisionPark :exec
INSERT INTO issue_decision_park (issue_id, workspace_id, prior_status, to_status, readiness_id, prior_readiness, set_value)
VALUES (@issue_id, @workspace_id, @prior_status, @to_status, sqlc.narg('readiness_id'), sqlc.narg('prior_readiness'), sqlc.narg('set_value'))
ON CONFLICT (issue_id) DO UPDATE SET
    prior_status = EXCLUDED.prior_status, to_status = EXCLUDED.to_status,
    readiness_id = EXCLUDED.readiness_id, prior_readiness = EXCLUDED.prior_readiness,
    set_value = EXCLUDED.set_value;

-- name: GetIssueDecisionPark :one
SELECT * FROM issue_decision_park WHERE issue_id = @issue_id AND workspace_id = @workspace_id;

-- name: DeleteIssueDecisionPark :exec
DELETE FROM issue_decision_park WHERE issue_id = @issue_id AND workspace_id = @workspace_id;
