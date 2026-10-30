-- name: LockIssueRunBudget :one
-- Serializes admission and grants on one issue: held to the end of the
-- caller's transaction, so the balance read and the debit cannot interleave.
SELECT id FROM issue WHERE id = sqlc.arg('issue_id')::uuid FOR NO KEY UPDATE;

-- name: CountIssueRunsSinceExcept :one
-- CountIssueRunsSince without one task: the window credit must not count the
-- run its own debit pays for.
SELECT COUNT(*)::bigint FROM agent_task_queue
WHERE issue_id = sqlc.arg('issue_id')::uuid AND created_at > sqlc.arg('since')::timestamptz
  AND id IS DISTINCT FROM sqlc.narg('except_task_id')::uuid;

-- name: InsertIssueRunBudgetEntry :exec
-- A replayed event (same issue, kind, key) is a no-op.
INSERT INTO issue_run_budget_entry (id, workspace_id, issue_id, window_id, kind, event_key, runs_delta)
VALUES (sqlc.arg('id'), sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('window_id'), sqlc.arg('kind'), sqlc.arg('event_key'), sqlc.arg('runs_delta'))
ON CONFLICT (issue_id, kind, event_key) DO NOTHING;

-- name: SumIssueRunBudgetWindow :one
SELECT COALESCE(SUM(runs_delta), 0)::bigint FROM issue_run_budget_entry
WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text;
