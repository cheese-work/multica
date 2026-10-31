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

-- name: InsertIssueRunBudgetTokenEntry :exec
-- Token side of the ledger (L2b): reservations, releases and usage charges. A
-- replayed event (same issue, kind, key) is a no-op.
INSERT INTO issue_run_budget_entry (id, workspace_id, issue_id, window_id, kind, event_key, tokens_delta)
VALUES (sqlc.arg('id'), sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('window_id'), sqlc.arg('kind'), sqlc.arg('event_key'), sqlc.arg('tokens_delta'))
ON CONFLICT (issue_id, kind, event_key) DO NOTHING;

-- name: GetIssueRunBudgetEntry :one
SELECT window_id, tokens_delta FROM issue_run_budget_entry
WHERE issue_id = sqlc.arg('issue_id')::uuid AND kind = sqlc.arg('kind')::text AND event_key = sqlc.arg('event_key')::text;

-- name: SumIssueRunBudgetTaskTokens :one
-- What a task's usage charges already hold: their sum and count.
SELECT COALESCE(SUM(tokens_delta), 0)::bigint AS charged, COUNT(*)::bigint AS entries
FROM issue_run_budget_entry
WHERE issue_id = sqlc.arg('issue_id')::uuid AND kind = 'tokens' AND event_key LIKE sqlc.arg('key_prefix')::text || '%';

-- name: ListIssueTaskIDsWithUsageSince :many
-- The tasks of an issue created at or after a time that already reported
-- usage: what token activation settles.
SELECT t.id FROM agent_task_queue t
WHERE t.issue_id = sqlc.arg('issue_id')::uuid AND t.created_at >= sqlc.arg('since')::timestamptz
  AND EXISTS (SELECT 1 FROM task_usage u WHERE u.task_id = t.id)
ORDER BY t.created_at, t.id;

-- name: SumIssueRunBudgetTokens :one
-- The issue's token balance: every token entry, in any window.
SELECT COALESCE(SUM(tokens_delta), 0)::bigint FROM issue_run_budget_entry
WHERE issue_id = sqlc.arg('issue_id')::uuid;

-- name: SumIssueRunBudgetWindowAllowance :one
-- The token allowance a window holds (credited once, by activation or by the
-- window's opening).
SELECT COALESCE(SUM(tokens_delta), 0)::bigint FROM issue_run_budget_entry
WHERE issue_id = sqlc.arg('issue_id')::uuid AND window_id = sqlc.arg('window_id')::text AND kind = 'allowance';

-- name: InsertIssueRunBudgetGrant :exec
-- A progress grant or refund (L2c): runs and tokens in one row, no cap change.
-- A replayed event (same issue, kind, key) is a no-op.
INSERT INTO issue_run_budget_entry (id, workspace_id, issue_id, window_id, kind, event_key, runs_delta, tokens_delta, cap_delta)
VALUES (sqlc.arg('id'), sqlc.arg('workspace_id'), sqlc.arg('issue_id'), sqlc.arg('window_id'), sqlc.arg('kind'), sqlc.arg('event_key'), sqlc.arg('runs_delta'), sqlc.arg('tokens_delta'), 0)
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
    AND created_at >= sqlc.arg('since')::timestamptz AND details->>'to' = sqlc.arg('status')::text
);
