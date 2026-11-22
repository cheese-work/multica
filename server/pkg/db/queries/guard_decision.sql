-- name: InsertGuardDecision :exec
-- One row of a daemon upload of a run's decision log. (task_id, seq) is unique,
-- so a retried upload adds nothing.
INSERT INTO guard_decision (workspace_id, task_id, seq, decided_at, plugin, action, tool, subcommand, line, issue_key, note, runtime)
VALUES (@workspace_id, @task_id, @seq, @decided_at, @plugin, @action, @tool, @subcommand, @line, @issue_key, @note, @runtime)
ON CONFLICT (task_id, seq) DO NOTHING;

-- name: ListGuardDecisions :many
-- Newest first, served by idx_guard_decision_workspace_at.
SELECT id, task_id, decided_at, plugin, action, tool, subcommand, line, issue_key, note, runtime
FROM guard_decision
WHERE workspace_id = @workspace_id
  AND (sqlc.narg('plugin')::text IS NULL OR plugin = sqlc.narg('plugin')::text)
  AND (sqlc.narg('action')::text IS NULL OR action = sqlc.narg('action')::text)
  AND decided_at >= @since
ORDER BY decided_at DESC, id DESC
LIMIT @row_limit;

-- name: GuardDecisionDaily :many
-- Per plugin, per viewer-timezone day, per action: the numbers on the Guard
-- settings page and in `multica guard stats`.
SELECT plugin, (decided_at AT TIME ZONE @tz::text)::date AS day, action, count(*)::bigint AS hits
FROM guard_decision
WHERE workspace_id = @workspace_id
  AND decided_at >= @since
GROUP BY plugin, day, action
ORDER BY plugin, day;
