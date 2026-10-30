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
INSERT INTO issue_run_budget_entry (id, workspace_id, issue_id, window_id, kind, event_key, runs_delta, cap_delta)
VALUES (sqlc.arg('id'), sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('window_id'), sqlc.arg('kind'), sqlc.arg('event_key'), sqlc.arg('runs_delta'), sqlc.arg('cap_delta'))
ON CONFLICT (issue_id, kind, event_key) DO NOTHING;

-- name: SumIssueRunBudgetWindow :one
SELECT COALESCE(SUM(runs_delta), 0)::bigint FROM issue_run_budget_entry
WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text;

-- name: SumIssueRunBudgetCap :one
-- The cap the window's ledger holds: credited by the window, raises and cap edits.
SELECT COALESCE(SUM(cap_delta), 0)::bigint FROM issue_run_budget_entry
WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text;

-- name: HasIssueRunBudgetWindow :one
SELECT EXISTS (
  SELECT 1 FROM issue_run_budget_entry
  WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text AND kind = 'window'
);
