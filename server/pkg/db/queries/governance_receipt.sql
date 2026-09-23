-- name: InsertGovernanceReceipt :one
-- One row per observation attempt (CHE-685). Plain INSERT, no upsert: a
-- receipt records one attempt, not a mutable per-issue/comment cursor, so
-- repeated observation (e.g. create then a later edit) simply accumulates
-- more rows rather than overwriting an earlier attempt's record.
INSERT INTO governance_receipt (
    workspace_id, issue_id, comment_id, trigger, status, shed_reason,
    abstain_reason, action_kind, answers, observed_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING *;

-- name: ListGovernanceReceiptsForComment :many
-- Test/diagnostic read: every observation attempt recorded for one comment,
-- newest first.
SELECT * FROM governance_receipt
WHERE comment_id = $1
ORDER BY created_at DESC;
