-- name: ListRunBudgetWindowRuns :many
-- The finished runs of an issue's open window, for the auto-approver. A run
-- delivered when it completed and its agent commented on the issue while it ran.
SELECT t.id, t.status, COALESCE(t.error, '')::text AS error, COALESCE(a.model, '')::text AS model,
       (t.status = 'completed' AND EXISTS (
           SELECT 1 FROM comment c
           WHERE c.issue_id = t.issue_id AND c.author_type = 'agent' AND c.author_id = t.agent_id
             AND c.created_at >= COALESCE(t.started_at, t.created_at)
             AND c.created_at <= COALESCE(t.completed_at, now()) + interval '1 minute'
       ))::boolean AS delivered,
       COALESCE((SELECT SUM(u.input_tokens + u.output_tokens) FROM task_usage u WHERE u.task_id = t.id), 0)::bigint AS tokens
FROM agent_task_queue t
JOIN agent a ON a.id = t.agent_id
WHERE t.issue_id = sqlc.arg('issue_id')::uuid AND t.created_at > sqlc.arg('since')::timestamptz
  AND t.status IN ('completed', 'failed', 'cancelled');

-- name: WorkspaceTaskTokensP50 :one
-- Median tokens per finished run in the workspace since a time; 0 when no run reported usage.
SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY s.tokens), 0)::float8 AS p50
FROM (
    SELECT SUM(u.input_tokens + u.output_tokens) AS tokens
    FROM task_usage u
    JOIN agent_task_queue t ON t.id = u.task_id
    JOIN agent a ON a.id = t.agent_id
    WHERE a.workspace_id = sqlc.arg('workspace_id')::uuid AND t.created_at > sqlc.arg('since')::timestamptz
    GROUP BY u.task_id
) s;

-- name: CountRunBudgetTopUpsSince :one
SELECT COUNT(*)::bigint FROM activity_log
WHERE issue_id = sqlc.arg('issue_id')::uuid AND action = 'run_budget_top_up' AND created_at > sqlc.arg('since')::timestamptz;

-- name: SumAutoApprovedRunsSince :one
-- Extra runs the auto-approver granted in the workspace since a time.
SELECT COALESCE(SUM((details->>'runs')::bigint), 0)::bigint FROM activity_log
WHERE workspace_id = sqlc.arg('workspace_id')::uuid AND action = 'run_budget_top_up'
  AND details ? 'auto' AND created_at >= sqlc.arg('since')::timestamptz;

-- name: LockRunBudgetAutoEnvelope :exec
-- Serializes auto-approvals of one workspace so the daily envelope is not overspent.
SELECT pg_advisory_xact_lock(hashtextextended('run_budget_auto:' || sqlc.arg('workspace_id')::text, 0));

-- name: ListRunBudgetAutoDecisionsSince :many
-- Every auto decision since a time, for the brief: approved top-ups and escalations by anomaly kind.
SELECT CASE WHEN action = 'run_budget_top_up' THEN 'approved' ELSE COALESCE(details->>'kind', 'unknown') END::text AS kind,
       COUNT(*)::bigint AS n
FROM activity_log
WHERE workspace_id = sqlc.arg('workspace_id')::uuid AND created_at >= sqlc.arg('since')::timestamptz
  AND ((action = 'run_budget_top_up' AND details ? 'auto') OR action = 'run_budget_auto_escalated')
GROUP BY 1;

-- name: CountWorkspaceDecisionsByFingerprint :one
-- Requests the workspace already holds under one server fingerprint; the anomaly batch card opens once per day.
SELECT COUNT(*)::bigint FROM decision_request
WHERE workspace_id = sqlc.arg('workspace_id')::uuid AND request_fingerprint = sqlc.arg('fingerprint')::text;
