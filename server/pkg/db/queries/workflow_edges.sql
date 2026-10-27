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
-- the snapshot belongs to the PR's newest head.
SELECT pr.state::text AS state,
       COALESCE(pr.checks_rollup_state, '')::text AS checks_rollup_state,
       (pr.snapshot_head_sha <> '' AND pr.snapshot_head_sha = pr.head_sha)::boolean AS head_fresh,
       pr.snapshot_head_sha::text AS head_sha
FROM github_pull_request pr
JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = ANY(sqlc.arg('issue_ids')::uuid[])
UNION ALL
SELECT pr.state::text, ''::text, false, ''::text
FROM vcs_pull_request pr
JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = ANY(sqlc.arg('issue_ids')::uuid[]);

-- name: CountIssueRunsSince :one
-- Runs that count against an issue's run budget: every task created after the
-- window start, whatever became of it.
SELECT COUNT(*)::bigint FROM agent_task_queue
WHERE issue_id = sqlc.arg('issue_id')::uuid AND created_at > sqlc.arg('since')::timestamptz;

-- name: LatestMemberStatusChange :one
-- When a person last changed the issue's status: a person who moves a blocked
-- issue on starts its run budget over.
SELECT created_at FROM activity_log
WHERE issue_id = sqlc.arg('issue_id')::uuid AND action = 'status_changed' AND actor_type = 'member'
ORDER BY created_at DESC LIMIT 1;

-- name: LockIssueReviewAdmission :exec
-- Serialises the review-admission check for one issue until the caller's
-- transaction ends, so two concurrent enqueues cannot both take the same head.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg('key')::text, 0));

-- name: HasUnreviewedPRHead :one
-- True when an open or draft PR linked to the issue sits on a head that no
-- run has been admitted for yet (the issue_review_admission trigger records
-- the admission when the run is created).
SELECT EXISTS (
    SELECT 1
    FROM issue_pull_request ipr
    JOIN github_pull_request pr ON pr.id = ipr.pull_request_id
    WHERE ipr.issue_id = sqlc.arg('issue_id')::uuid
      AND pr.state IN ('open', 'draft') AND pr.head_sha <> ''
      AND NOT EXISTS (
          SELECT 1 FROM issue_review_admission a
          WHERE a.issue_id = ipr.issue_id AND a.pull_request_id = pr.id AND a.head_sha = pr.head_sha
      )
)::boolean;

-- name: RaiseIssueMaxRuns :one
-- Sets the issue's own run cap, only upward from the cap now in force.
UPDATE issue SET max_runs_override = sqlc.arg('runs')::int
WHERE id = sqlc.arg('id')::uuid AND workspace_id = sqlc.arg('workspace_id')::uuid
  AND sqlc.arg('runs')::int > COALESCE(max_runs_override, sqlc.arg('workflow_max')::int)
RETURNING max_runs_override;
