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

-- name: ListRunBudgetBlockedIssueIDs :many
-- Issues the run budget holds in blocked (CHE-5285). The marker exists only
-- while the issue is blocked (trigger in migration 882), so the set is small.
SELECT issue_id FROM run_budget_notice
WHERE workspace_id = $1 AND kind = 'blocked';

-- name: FindOpenRunBudgetCard :one
-- The open raise or top-up card of one issue's budget window, whoever opened it
-- (CHE-5503): a refusal's server card, a member's request or an agent's
-- proposal. Oldest first, so every reader names the same card.
SELECT r.id AS request_id, a.id AS action_id, a.action_type
FROM decision_request r
JOIN decision_action a ON a.decision_request_id = r.id AND a.workspace_id = r.workspace_id
WHERE r.workspace_id = sqlc.arg('workspace_id') AND r.issue_id = sqlc.arg('issue_id') AND r.status = 'open'
  AND a.state = 'pending' AND a.action_type IN ('issue.run_budget.raise', 'issue.run_budget.top_up')
  AND a.params->>'window_id' = sqlc.arg('window_id')::text
ORDER BY r.created_at ASC, r.id ASC
LIMIT 1;
