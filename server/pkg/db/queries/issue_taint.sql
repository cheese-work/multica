-- name: LockIssueForTaint :exec
-- Share lock: a taint writer holds the issue row (issue_taint_note), so an
-- authorization waits for it and one that starts after sees the new taint.
SELECT 1 FROM issue WHERE id = $1 FOR SHARE;

-- name: LockIssueForTaintClear :exec
SELECT 1 FROM issue WHERE id = $1 FOR NO KEY UPDATE;

-- name: GetIssueTaintIDs :one
SELECT COALESCE((SELECT taint_ids FROM issue_taint WHERE issue_id = $1), '{}')::text[] AS taint_ids;

-- name: ClearIssueTaintIDs :exec
UPDATE issue_taint SET updated_at = clock_timestamp(),
    taint_ids = ARRAY(SELECT t FROM unnest(taint_ids) AS t WHERE t <> ALL (sqlc.arg('cleared')::text[]))
WHERE issue_id = sqlc.arg('issue_id');

-- name: InsertIssueTaintClear :one
INSERT INTO issue_taint_clear (workspace_id, issue_id, taint_ids, cleared_by_member_id)
VALUES ($1, $2, $3, $4)
RETURNING *;
