-- name: ListRunBudgetWindowRuns :many
-- The finished runs of an issue's open window, for the auto-approver, from
-- structural evidence only; no comment text is read. A run delivered when it
-- completed and, while it ran, one of: the agent moved the issue's status; a
-- linked PR's head, state or draft flag changed (an issue_pr_event, so a PR
-- comment does not count); or the run's shell committed (a git commit result in
-- its tool output, so no PR is needed). failures are the run's loop keys beyond
-- its own error: the failed check names and the CI failure workflows recorded
-- for the heads it pushed, so a loop shows across completed runs. ci_unknown
-- fails closed: it is set unless each head the run pushed holds a finished row
-- for every name in the expected check set (expected_checks, the workspace
-- setting) and no row still queued or running. A stored snapshot alone is not
-- complete: a check that has not arrived yet has no row. An empty expected set
-- is no evidence, so every pushed head reads as unknown. A head with no retained
-- check results (they are kept only for a PR's current head, and CI failure
-- events only while the ci_watcher flag is on) says nothing final either.
SELECT t.id, t.status, COALESCE(NULLIF(t.error, ''), t.failure_reason, '')::text AS error, COALESCE(a.model, '')::text AS model,
       (t.status = 'completed' AND (
           EXISTS (
               SELECT 1 FROM issue_pr_event e
               WHERE e.issue_id = t.issue_id AND e.kind IN ('head_changed', 'ready', 'draft', 'merged', 'closed')
                 AND e.observed_at >= COALESCE(t.started_at, t.created_at) AND e.observed_at <= (COALESCE(t.completed_at, now()) + interval '1 minute')
           )
           OR EXISTS (
               SELECT 1 FROM task_message m
               WHERE m.task_id = t.id AND m.type = 'tool_result'
                 AND m.output ~ '(^|\n)\[[^\]\n]+ [0-9a-f]{7,40}\] '
           )
           OR EXISTS (
               SELECT 1 FROM activity_log al
               WHERE al.issue_id = t.issue_id AND al.action = 'status_changed'
                 AND al.actor_type = 'agent' AND al.actor_id = t.agent_id
                 AND al.created_at >= COALESCE(t.started_at, t.created_at) AND al.created_at <= (COALESCE(t.completed_at, now()) + interval '1 minute')
           )
       ))::boolean AS delivered,
       COALESCE((
           SELECT array_agg(DISTINCT f.k) FROM (
               SELECT 'check:' || cr.name AS k
               FROM issue_pr_event e
               JOIN github_pull_request_check_run cr ON cr.pr_id = e.pull_request_id AND cr.head_sha = e.head_sha
               WHERE e.issue_id = t.issue_id AND e.kind = 'head_changed'
                 AND e.observed_at >= COALESCE(t.started_at, t.created_at) AND e.observed_at <= (COALESCE(t.completed_at, now()) + interval '1 minute')
                 AND cr.conclusion IN ('failure', 'timed_out', 'startup_failure')
               UNION ALL
               SELECT 'ci:' || cf.workflow_name AS k
               FROM issue_pr_event e
               JOIN ci_failure_event cf ON cf.workspace_id = e.workspace_id AND cf.head_sha = e.head_sha
               WHERE e.issue_id = t.issue_id AND e.kind = 'head_changed'
                 AND e.observed_at >= COALESCE(t.started_at, t.created_at) AND e.observed_at <= (COALESCE(t.completed_at, now()) + interval '1 minute')
                 AND cf.workflow_name <> ''
           ) f
       ), '{}')::text[] AS failures,
       EXISTS (
           SELECT 1 FROM issue_pr_event e
           WHERE e.issue_id = t.issue_id AND e.kind = 'head_changed'
             AND e.observed_at >= COALESCE(t.started_at, t.created_at) AND e.observed_at <= (COALESCE(t.completed_at, now()) + interval '1 minute')
             AND (
                 cardinality(sqlc.arg('expected_checks')::text[]) = 0
                 OR EXISTS (
                     SELECT 1 FROM unnest(sqlc.arg('expected_checks')::text[]) x(name)
                     WHERE NOT EXISTS (
                         SELECT 1 FROM github_pull_request_check_run cr
                         WHERE cr.pr_id = e.pull_request_id AND cr.head_sha = e.head_sha AND cr.name = x.name
                           AND cr.status = 'completed' AND cr.conclusion IS NOT NULL
                     )
                 )
                 OR EXISTS (SELECT 1 FROM github_pull_request_check_run cr WHERE cr.pr_id = e.pull_request_id AND cr.head_sha = e.head_sha AND (cr.status <> 'completed' OR cr.conclusion IS NULL))
             )
       )::boolean AS ci_unknown,
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

-- name: ListWorkspaceDecisionsByFingerprint :many
-- The requests the workspace already holds under one server fingerprint (any status): the anomaly batch card opens once per day, and later anomalies go to its chat sessions.
SELECT chat_session_id, asking_agent_id, human_user_id FROM decision_request
WHERE workspace_id = sqlc.arg('workspace_id')::uuid AND request_fingerprint = sqlc.arg('fingerprint')::text;

-- name: ListRunBudgetAutoEscalationsSince :many
-- The issues the auto-approver escalated since a time, newest first, for the brief.
SELECT COALESCE(a.details->>'kind', 'unknown')::text AS kind, i.number::bigint AS number
FROM activity_log a JOIN issue i ON i.id = a.issue_id
WHERE a.workspace_id = sqlc.arg('workspace_id')::uuid AND a.action = 'run_budget_auto_escalated'
  AND a.created_at >= sqlc.arg('since')::timestamptz
ORDER BY a.created_at DESC
LIMIT 10;
