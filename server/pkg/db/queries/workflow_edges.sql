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
SELECT pr.state::text AS state,
       COALESCE(pr.checks_rollup_state, '')::text AS checks_rollup_state,
       (pr.snapshot_head_sha <> '' AND pr.snapshot_head_sha = pr.head_sha)::boolean AS head_fresh,
       pr.snapshot_head_sha::text AS head_sha,
       (CASE WHEN pr.review_head_sha = pr.head_sha THEN pr.review_state ELSE '' END)::text AS review
FROM github_pull_request pr
JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = ANY(sqlc.arg('issue_ids')::uuid[])
UNION ALL
SELECT pr.state::text, ''::text, false, ''::text, ''::text
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
-- When a person last changed the issue's status: a person who moves a blocked
-- issue on starts its run budget over.
SELECT created_at FROM activity_log
WHERE issue_id = sqlc.arg('issue_id')::uuid AND action = 'status_changed' AND actor_type = 'member'
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

-- name: ListMemberStatusChangesAfter :many
-- Every time a person moved the issue's status after a point: each one opened a
-- run-budget window, whether or not anything was admitted in it.
SELECT created_at FROM activity_log
WHERE issue_id = sqlc.arg('issue_id')::uuid AND action = 'status_changed' AND actor_type = 'member'
  AND created_at > sqlc.arg('after')::timestamptz
ORDER BY created_at;
