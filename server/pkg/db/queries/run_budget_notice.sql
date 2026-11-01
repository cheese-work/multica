-- name: ClaimRunBudgetNotice :one
-- Claims the notice of one kind for one budget window (CHE-2297 L1a). A lost
-- race returns pgx.ErrNoRows: only the inserting transaction writes the
-- comment and the inbox item.
INSERT INTO run_budget_notice (id, workspace_id, issue_id, window_id, kind)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (issue_id, window_id, kind) DO NOTHING
RETURNING id;

-- name: TakeOverRunBudgetNotice :one
-- Takes a claim whose holder is presumed gone: older than the lease. The new
-- id makes the old holder's release match nothing. No row means the claim is
-- fresh or absent (pgx.ErrNoRows).
UPDATE run_budget_notice SET id = $1, created_at = now()
WHERE issue_id = $2 AND window_id = $3 AND kind = $4
  AND created_at < now() - make_interval(secs => sqlc.arg(lease_seconds)::float8)
RETURNING id;

-- name: ReleaseRunBudgetNotice :exec
-- Gives a claim back. Only the claim of a window's exhaustion handling is
-- released: it orders concurrent callbacks, not notices.
DELETE FROM run_budget_notice WHERE id = $1;

-- name: SetRunBudgetNoticeComment :exec
UPDATE run_budget_notice SET comment_id = $2 WHERE id = $1;

-- name: ListWorkspaceOwnerUserIDs :many
SELECT user_id FROM member
WHERE workspace_id = $1 AND role = 'owner'
ORDER BY created_at ASC, id ASC;
