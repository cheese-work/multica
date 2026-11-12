-- name: ListRunBudgetWindowRuns :many
-- The finished runs of an issue's open window, for the auto-approver. A run
-- delivered when it completed and, while it ran, one of: the agent moved the
-- issue's status; a linked PR's head, state or draft flag changed (an
-- issue_pr_event, so a PR comment does not count); the run's shell committed (a
-- git commit result in its tool output, so no PR is needed); or the agent
-- commented that it did something and did not say it is stuck. A comment that
-- is stuck, a no-op, or says neither is not delivery. evidence is every
-- failure-reporting comment of the run, which the engine reduces to the failing
-- checks and errors, so a loop shows across completed runs, not only failed ones.
SELECT t.id, t.status, COALESCE(t.error, '')::text AS error, COALESCE(a.model, '')::text AS model,
       (t.status = 'completed' AND (
           EXISTS (
               SELECT 1 FROM comment c
               WHERE c.issue_id = t.issue_id AND c.author_type = 'agent' AND c.author_id = t.agent_id
                 AND c.created_at >= COALESCE(t.started_at, t.created_at) AND c.created_at <= (COALESCE(t.completed_at, now()) + interval '1 minute')
                 AND c.content ~* '\m(done|completed?|implemented|fixed|added|merged|pushed|committed|opened|created|updated|resolved|shipped|delivered|released|refactored|removed|renamed|wrote|published)\M'
                 AND c.content !~* '(no[ -]?op|nothing (to do|changed|new|left)|no (further|new|more) (action|work|changes?|progress)|no changes?( needed| required)?|already (done|complete|up to date)|\m(still|remains?|remaining|is|am|are) (blocked|stuck)\M|\m(blocked|stuck|waiting|awaiting|unable|cannot|can''t)\M|need(s|ed)? (input|a decision|approval|clarification|access))'
           )
           OR EXISTS (
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
           SELECT left(string_agg(c.content, E'\n' ORDER BY c.created_at), 20000) FROM comment c
           WHERE c.issue_id = t.issue_id AND c.author_type = 'agent' AND c.author_id = t.agent_id
             AND c.created_at >= COALESCE(t.started_at, t.created_at) AND c.created_at <= (COALESCE(t.completed_at, now()) + interval '1 minute')
             AND c.content ~* '\m(fail(ed|ing|s|ure)?|error|red)\M'
       ), '')::text AS evidence,
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
