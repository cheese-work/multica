-- Run-budget before/after measure (CHE-5507, slice S6 of CHE-5236). Three
-- queries, one row per ICT day (Asia/Bangkok, 00:00-24:00) of the window:
--   M1  issues blocked by the run budget, and the hours they stayed blocked
--   M2  budget asks (decision_action), executions, origin and approver
--   M3  agent runs per merged PR
-- A day before a source's first row on the workspace is "unavailable": the
-- columns are NULL, never 0. Report the covered days next to each result.
--
--   psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 -f scripts/run-budget-measure.sql \
--     -v workspace_id=<uuid> -v from_day=2026-10-28 -v to_day=2026-11-10 \
--     -v report_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
--
-- The `-- @query` lines mark each query for the test; psql ignores them.
-- Cleanup: remove this file and the brief lines at the end date (CHE-5236).

-- @query M1
-- Block = activity_log status_changed to blocked with cause run_budget. Hours =
-- the gap to the issue's next status_changed from blocked; an issue still
-- blocked counts to :report_time. run_budget_notice kind blocked is not used
-- (migration 882 deletes it when the issue leaves blocked). Exhausted notices
-- are kept, so they are the cross-check column.
WITH win AS (
  SELECT d::date AS day
  FROM generate_series(:'from_day'::date, :'to_day'::date, interval '1 day') AS d
),
blocks AS (
  SELECT a.issue_id, a.created_at, (a.created_at AT TIME ZONE 'Asia/Bangkok')::date AS day
  FROM activity_log a
  WHERE a.workspace_id = :'workspace_id'::uuid
    AND a.action = 'status_changed'
    AND a.details->>'to' = 'blocked'
    AND a.details->'trigger'->'cause'->>'kind' = 'run_budget'
),
spans AS (
  SELECT b.issue_id, b.day,
         GREATEST(0, EXTRACT(EPOCH FROM
           LEAST(COALESCE(x.exited_at, :'report_time'::timestamptz), :'report_time'::timestamptz) - b.created_at) / 3600) AS hours
  FROM blocks b
  LEFT JOIN LATERAL (
    SELECT min(e.created_at) AS exited_at
    FROM activity_log e
    WHERE e.issue_id = b.issue_id AND e.action = 'status_changed'
      AND e.details->>'from' = 'blocked' AND e.created_at > b.created_at
  ) x ON true
),
per_day AS (
  SELECT day, count(DISTINCT issue_id) AS issues, count(*) AS occurrences, sum(hours) AS hours
  FROM spans GROUP BY day
),
exhausted AS (
  SELECT (created_at AT TIME ZONE 'Asia/Bangkok')::date AS day, count(DISTINCT issue_id) AS issues
  FROM run_budget_notice
  WHERE workspace_id = :'workspace_id'::uuid AND kind = 'exhausted'
  GROUP BY 1
),
first_rows AS (
  SELECT (SELECT min(day) FROM blocks) AS block_day,
         (SELECT min(day) FROM exhausted) AS exhausted_day
)
SELECT w.day,
       COALESCE(f.block_day <= w.day, false) AS covered,
       CASE WHEN f.block_day <= w.day THEN COALESCE(p.issues, 0) END AS blocked_issues,
       CASE WHEN f.block_day <= w.day THEN COALESCE(p.occurrences, 0) END AS blocked_occurrences,
       CASE WHEN f.block_day <= w.day THEN round(COALESCE(p.hours, 0), 2) END AS blocked_hours,
       CASE WHEN f.exhausted_day <= w.day THEN COALESCE(x.issues, 0) END AS exhausted_notice_issues,
       CASE WHEN w.day = DATE '2026-11-13' THEN 'CHE-5303 change date' END AS mark
FROM win w
CROSS JOIN first_rows f
LEFT JOIN per_day p ON p.day = w.day
LEFT JOIN exhausted x ON x.day = w.day
ORDER BY w.day;

-- @query M2
-- Asks per ICT day of created_at, by action type; executed per ICT day of
-- executed_at with state executed. Origin is the actor type of the
-- run_budget_ask activity row (S2, CHE-5503): unavailable before its first row.
-- The auto-approver (CHE-5331) writes no decision_action; its top-up is the
-- run_budget_top_up activity row that carries an `auto` key, so executed_by_auto
-- is unavailable until the first such row (CHE-5379 enables it).
WITH win AS (
  SELECT d::date AS day
  FROM generate_series(:'from_day'::date, :'to_day'::date, interval '1 day') AS d
),
types(action_type) AS (
  VALUES ('issue.run_budget.raise'), ('issue.run_budget.top_up'),
         ('workflow.budget.set'), ('issue.workflow.set')
),
acts AS (
  SELECT action_type, state, webauthn_credential_id,
         (created_at AT TIME ZONE 'Asia/Bangkok')::date AS asked_day,
         (executed_at AT TIME ZONE 'Asia/Bangkok')::date AS executed_day
  FROM decision_action
  WHERE workspace_id = :'workspace_id'::uuid
    AND action_type IN (SELECT action_type FROM types)
),
asked AS (
  SELECT action_type, asked_day AS day, count(*) AS asks FROM acts GROUP BY 1, 2
),
executed AS (
  SELECT action_type, executed_day AS day, count(*) AS executed,
         count(*) FILTER (WHERE webauthn_credential_id IS NOT NULL) AS by_passkey
  FROM acts WHERE state = 'executed' GROUP BY 1, 2
),
origin AS (
  SELECT details->>'action_type' AS action_type, (created_at AT TIME ZONE 'Asia/Bangkok')::date AS day,
         count(*) FILTER (WHERE actor_type = 'agent') AS by_agent,
         count(*) FILTER (WHERE actor_type = 'member') AS by_member,
         count(*) FILTER (WHERE actor_type = 'system') AS by_system
  FROM activity_log
  WHERE workspace_id = :'workspace_id'::uuid AND action = 'run_budget_ask'
  GROUP BY 1, 2
),
auto AS (
  SELECT (created_at AT TIME ZONE 'Asia/Bangkok')::date AS day, count(*) AS top_ups
  FROM activity_log
  WHERE workspace_id = :'workspace_id'::uuid AND action = 'run_budget_top_up' AND details ? 'auto'
  GROUP BY 1
),
first_rows AS (
  SELECT (SELECT min(asked_day) FROM acts) AS ask_day,
         (SELECT min(day) FROM origin) AS origin_day,
         (SELECT min(day) FROM auto) AS auto_day
)
SELECT w.day, t.action_type,
       CASE WHEN f.ask_day <= w.day THEN COALESCE(a.asks, 0) END AS asks,
       CASE WHEN f.ask_day <= w.day THEN COALESCE(e.executed, 0) END AS executed,
       CASE WHEN f.ask_day <= w.day THEN COALESCE(e.by_passkey, 0) END AS executed_by_passkey,
       CASE WHEN f.ask_day <= w.day THEN COALESCE(e.executed, 0) - COALESCE(e.by_passkey, 0) END AS executed_other,
       CASE
         WHEN t.action_type <> 'issue.run_budget.top_up' THEN 0
         WHEN f.auto_day <= w.day THEN COALESCE(u.top_ups, 0)
       END AS executed_by_auto,
       COALESCE(f.origin_day <= w.day, false) AS origin_available,
       CASE WHEN f.origin_day <= w.day THEN COALESCE(o.by_agent, 0) END AS asks_by_agent,
       CASE WHEN f.origin_day <= w.day THEN COALESCE(o.by_member, 0) END AS asks_by_member,
       CASE WHEN f.origin_day <= w.day THEN COALESCE(o.by_system, 0) END AS asks_by_system,
       CASE WHEN w.day = DATE '2026-11-13' THEN 'CHE-5303 change date' END AS mark
FROM win w
CROSS JOIN types t
CROSS JOIN first_rows f
LEFT JOIN asked a ON a.day = w.day AND a.action_type = t.action_type
LEFT JOIN executed e ON e.day = w.day AND e.action_type = t.action_type
LEFT JOIN origin o ON o.day = w.day AND o.action_type = t.action_type
LEFT JOIN auto u ON u.day = w.day
ORDER BY w.day, t.action_type;

-- @query M3
-- Merged PRs linked to an issue, per ICT day of merged_at, and the agent task
-- rows of the linked issues created before the merge. A task counts once, under
-- the first merged PR of its issue that merged after the task was created.
-- Days with no merged PR read "no PRs"; days before the first one are
-- "unavailable".
WITH win AS (
  SELECT d::date AS day
  FROM generate_series(:'from_day'::date, :'to_day'::date, interval '1 day') AS d
),
linked AS (
  SELECT DISTINCT l.issue_id, p.id AS pr_id, p.merged_at,
         (p.merged_at AT TIME ZONE 'Asia/Bangkok')::date AS day
  FROM github_pull_request p
  JOIN issue_pull_request l ON l.pull_request_id = p.id
  JOIN issue i ON i.id = l.issue_id AND i.workspace_id = :'workspace_id'::uuid
  WHERE p.workspace_id = :'workspace_id'::uuid AND p.state = 'merged' AND p.merged_at IS NOT NULL
),
attributed AS (
  SELECT DISTINCT ON (t.id) t.id AS task_id, k.day
  FROM agent_task_queue t
  JOIN linked k ON k.issue_id = t.issue_id AND k.merged_at > t.created_at
  ORDER BY t.id, k.merged_at, k.pr_id
),
prs AS (
  SELECT day, count(DISTINCT pr_id) AS merged_prs FROM linked GROUP BY day
),
runs AS (
  SELECT day, count(*) AS runs FROM attributed GROUP BY day
),
first_rows AS (
  SELECT min(day) AS pr_day FROM linked
)
SELECT w.day,
       CASE
         WHEN f.pr_day IS NULL OR w.day < f.pr_day THEN 'unavailable'
         WHEN COALESCE(p.merged_prs, 0) = 0 THEN 'no PRs'
         ELSE 'ok'
       END AS status,
       CASE WHEN f.pr_day <= w.day THEN COALESCE(p.merged_prs, 0) END AS merged_prs,
       CASE WHEN p.merged_prs > 0 THEN COALESCE(r.runs, 0) END AS runs,
       CASE WHEN p.merged_prs > 0 THEN round(COALESCE(r.runs, 0)::numeric / p.merged_prs, 2) END AS runs_per_pr,
       CASE WHEN w.day = DATE '2026-11-13' THEN 'CHE-5303 change date' END AS mark
FROM win w
CROSS JOIN first_rows f
LEFT JOIN prs p ON p.day = w.day
LEFT JOIN runs r ON r.day = w.day
ORDER BY w.day;
