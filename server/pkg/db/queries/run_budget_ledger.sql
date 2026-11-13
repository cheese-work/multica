-- name: LockIssueRunBudget :one
-- Serializes admission and grants on one issue: held to the end of the
-- caller's transaction, so the balance read and the debit cannot interleave.
SELECT id FROM issue WHERE id = sqlc.arg('issue_id')::uuid FOR NO KEY UPDATE;

-- name: LockIssueRunBudgetBoundary :exec
-- Orders the moves that start a run-budget window on one issue (a person's
-- status move, a top-up). An advisory lock, not the issue row: the move's own
-- write and its listeners update that row while the lock is held.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg('issue_id')::uuid::text || ':run_budget_boundary', 0));

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

-- name: SumIssueRunBudgetRuns :one
-- The issue's run balance: every run entry, in any window.
SELECT COALESCE(SUM(runs_delta), 0)::bigint FROM issue_run_budget_entry
WHERE issue_id = sqlc.arg('issue_id')::uuid;

-- name: SumIssueRunBudgetCap :one
-- The cap the window's ledger holds: credited by the window, raises and cap edits.
SELECT COALESCE(SUM(cap_delta), 0)::bigint FROM issue_run_budget_entry
WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text;

-- name: HasIssueRunBudgetWindow :one
SELECT EXISTS (
  SELECT 1 FROM issue_run_budget_entry
  WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text AND kind = 'window'
);

-- name: InsertIssueRunBudgetGrant :exec
-- A progress grant or refund (L2c): runs, no cap change.
-- A replayed event (same issue, kind, key) is a no-op.
INSERT INTO issue_run_budget_entry (id, workspace_id, issue_id, window_id, kind, event_key, runs_delta, cap_delta)
VALUES (sqlc.arg('id'), sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('window_id'), sqlc.arg('kind'), sqlc.arg('event_key'), sqlc.arg('runs_delta'), 0)
ON CONFLICT (issue_id, kind, event_key) DO NOTHING;

-- name: CountIssueRunBudgetGrants :one
-- The automatic grants a window already holds: the 3-per-window cap counts them.
SELECT COUNT(*)::bigint FROM issue_run_budget_entry
WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text
  AND kind IN ('step', 'head', 'stale_pr');

-- name: HasIssueRunBudgetEntryInWindow :one
SELECT EXISTS (
  SELECT 1 FROM issue_run_budget_entry
  WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text
    AND kind = sqlc.arg('kind')::text AND event_key = sqlc.arg('event_key')::text
);

-- name: HasRunBudgetNotice :one
-- Whether the window's notice of this kind was claimed: for kind exhausted, the
-- window is spent and stays closed to automatic grants until a person acts.
SELECT EXISTS (
  SELECT 1 FROM run_budget_notice
  WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text AND kind = sqlc.arg('kind')::text
);

-- name: LockRunBudgetBlockMarker :one
-- Whether the budget's block of the issue still stands in this window, holding
-- the marker so the move out of blocked (which deletes it) waits for the
-- caller's transaction.
SELECT id FROM run_budget_notice
WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text AND kind = 'blocked'
FOR SHARE;

-- name: FirstStatusInWindow :one
-- The status the issue held when the window opened: where the earliest status
-- move at or after the window start came from.
SELECT COALESCE(details->>'from', '')::text FROM activity_log
WHERE issue_id = sqlc.arg('issue_id')::uuid AND action = 'status_changed' AND created_at >= sqlc.arg('since')::timestamptz
ORDER BY created_at ASC LIMIT 1;

-- name: HasMemberMoveToStatusSince :one
-- Whether a person moved the issue into this status at or after the window
-- start: the person's target counts as visited.
SELECT EXISTS (
  SELECT 1 FROM activity_log
  WHERE issue_id = sqlc.arg('issue_id')::uuid AND action = 'status_changed' AND actor_type = 'member'
    AND details->'trigger'->'cause'->>'kind' IS DISTINCT FROM 'workflow_remap'
    AND created_at >= sqlc.arg('since')::timestamptz AND details->>'to' = sqlc.arg('status')::text
);

-- name: RunBudgetBoundaryClock :one
-- The time now, not the transaction's start: a window boundary taken after a
-- wait for the boundary lock must sort after every boundary written during it.
SELECT clock_timestamp()::timestamptz;
