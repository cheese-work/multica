-- The workflow_edges system rule (CHE-2152): one row per issue sitting on a
-- workflow step with fact-guarded edges. A trigger on issue.status owns its
-- life; these queries only reschedule or drop it and read the facts.

-- name: ScheduleWorkflowEdges :exec
UPDATE issue_wakeup SET next_fire_at = sqlc.arg('next_fire_at')::timestamptz, updated_at = clock_timestamp()
WHERE id = sqlc.arg('id')::uuid AND system_rule = 'workflow_edges';

-- name: DeleteWorkflowEdges :exec
DELETE FROM issue_wakeup WHERE id = sqlc.arg('id')::uuid AND system_rule = 'workflow_edges';

-- name: ListEdgePullRequests :many
-- Every PR linked to the given issues, GitHub and self-hosted, as the facts
-- edge guards read. Only a GitHub PR carries a checks snapshot; head_fresh says
-- the snapshot belongs to the PR's newest head; review is the latest human
-- review decision, and counts only while it is on the PR's current head.
-- pr_head_sha is the PR's own newest head, which a checks snapshot may not have
-- caught up with. red_cause is why the failing checks are red (CHE-3257):
-- 'pr' when any failing check is PR-caused; '' when nothing fails (or no
-- per-check rows exist yet); 'unknown' when a failing check has no cause; then
-- 'infra' or 'optional'. 'flaky' when nothing fails now but a check was marked
-- flaky on this head: red earlier, green on a rerun of the same head.
SELECT pr.state::text AS state,
       COALESCE(pr.checks_rollup_state, '')::text AS checks_rollup_state,
       (pr.snapshot_head_sha <> '' AND pr.snapshot_head_sha = pr.head_sha)::boolean AS head_fresh,
       pr.snapshot_head_sha::text AS head_sha,
       pr.head_sha::text AS pr_head_sha,
       (CASE WHEN pr.review_head_sha = pr.head_sha THEN pr.review_state ELSE '' END)::text AS review,
       (CASE
          WHEN f.failing = 0 AND f.flaky > 0 THEN 'flaky'
          WHEN f.failing = 0 THEN ''
          WHEN f.pr > 0 THEN 'pr'
          WHEN f.unknown > 0 THEN 'unknown'
          WHEN f.infra > 0 THEN 'infra'
          ELSE 'optional'
        END)::text AS red_cause
FROM github_pull_request pr
JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE c.failing) AS failing,
           count(*) FILTER (WHERE c.failing AND c.red_cause = 'pr') AS pr,
           count(*) FILTER (WHERE c.failing AND c.red_cause IS NULL) AS unknown,
           count(*) FILTER (WHERE c.failing AND c.red_cause = 'infra') AS infra,
           count(*) FILTER (WHERE NOT c.failing AND c.red_cause = 'flaky') AS flaky
    FROM (
        SELECT red_cause,
               (status = 'completed' AND conclusion IN ('failure', 'cancelled', 'timed_out', 'action_required', 'startup_failure', 'stale', 'error')) AS failing
        FROM github_pull_request_check_run
        WHERE pr_id = pr.id AND head_sha = pr.snapshot_head_sha
    ) c
) f
WHERE ipr.issue_id = ANY(sqlc.arg('issue_ids')::uuid[])
UNION ALL
SELECT pr.state::text, ''::text, false, ''::text, ''::text, ''::text, ''::text
FROM vcs_pull_request pr
JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = ANY(sqlc.arg('issue_ids')::uuid[]);

-- name: CountIssueRunsSince :one
-- Runs that count against an issue's run budget: every task created after the
-- window start, whatever became of it.
SELECT COUNT(*)::bigint FROM agent_task_queue
WHERE issue_id = sqlc.arg('issue_id')::uuid AND created_at > sqlc.arg('since')::timestamptz;

-- name: CurrentXactID :one
-- The current transaction's id, so a run-budget notice can wait for that
-- transaction to finish. The cast keeps the xid8 out of the Go type map.
SELECT pg_current_xact_id()::text::bigint;

-- name: XactStatus :one
-- 'in progress', 'committed', 'aborted', or 'unknown' once the id is too old.
SELECT COALESCE(pg_xact_status(sqlc.arg('xid')::bigint::text::xid8), 'unknown')::text;

-- name: LatestMemberStatusChange :one
-- When a person last changed the issue's status or topped up its run budget: a
-- person who moves a blocked issue on, or tops it up, starts its run budget over.
-- After the S1d cutover only an interactive session's ui_or_api move opens a
-- window; a PAT, cli_jwt, a row with no trigger and a remap never do.
SELECT created_at FROM activity_log
WHERE issue_id = sqlc.arg('issue_id')::uuid AND action IN ('status_changed', 'run_budget_top_up') AND (actor_type = 'member' OR (action = 'run_budget_top_up' AND actor_type = 'system'))
  AND details->'trigger'->'cause'->>'kind' IS DISTINCT FROM 'workflow_remap'
  AND (
    action = 'run_budget_top_up'
    OR created_at < (SELECT cutover_at FROM run_budget_window_cutover)
    OR (details->'trigger'->>'auth_source' = 'interactive_jwt' AND details->'trigger'->'cause'->>'kind' = 'ui_or_api')
  )
ORDER BY created_at DESC LIMIT 1;

-- name: LatestMemberStatusMove :one
-- When a person last moved the issue's status or topped up its run budget,
-- whatever session made the move. A refused dispatch resumes on any such move
-- (decision_wait.sql selects on the same rows); only the run-budget window is
-- limited to interactive sessions (LatestMemberStatusChange).
SELECT created_at FROM activity_log
WHERE issue_id = sqlc.arg('issue_id')::uuid AND action IN ('status_changed', 'run_budget_top_up') AND (actor_type = 'member' OR (action = 'run_budget_top_up' AND actor_type = 'system'))
  AND details->'trigger'->'cause'->>'kind' IS DISTINCT FROM 'workflow_remap'
ORDER BY created_at DESC LIMIT 1;

-- name: HasIssueReviewAdmission :one
-- Whether the issue's first review of this PR head was already admitted. The
-- row is written with the task row (CreateAgentTask), so it exists exactly when
-- a run was admitted as the reviewer for that head.
SELECT EXISTS (
    SELECT 1 FROM issue_review_admission
    WHERE issue_id = sqlc.arg('issue_id')::uuid AND head_sha = sqlc.arg('head_sha')::text
);

-- name: RaiseIssueMaxRuns :one
-- Sets the issue's own run cap, only upward from the cap now in force.
UPDATE issue SET max_runs_override = sqlc.arg('runs')::int
WHERE id = sqlc.arg('id')::uuid AND workspace_id = sqlc.arg('workspace_id')::uuid
  AND sqlc.arg('runs')::int > COALESCE(max_runs_override, sqlc.arg('workflow_max')::int)
RETURNING max_runs_override;

