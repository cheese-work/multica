-- name: ClaimRunBudgetNotice :one
-- Claims the notice of one kind for one budget window (CHE-2297 L1a). A lost
-- race returns pgx.ErrNoRows: only the inserting transaction writes the
-- comment and the inbox item.
INSERT INTO run_budget_notice (id, workspace_id, issue_id, window_id, kind)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (issue_id, window_id, kind) DO NOTHING
RETURNING id;

-- name: SetRunBudgetNoticeComment :exec
UPDATE run_budget_notice SET comment_id = $2 WHERE id = $1;

-- name: ListWorkspaceOwnerUserIDs :many
SELECT user_id FROM member
WHERE workspace_id = $1 AND role = 'owner'
ORDER BY created_at ASC, id ASC;
