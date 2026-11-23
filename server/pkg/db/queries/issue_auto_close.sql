-- name: GetLatestIssueVerdict :one
-- CHE-6969: the verdict of record is the newest verdict row; its details carry
-- the findings, the reviewed pull request and the head it names.
SELECT id, action, details, created_at, actor_id FROM activity_log
WHERE issue_id = $1 AND action IN ('review_passed', 'review_changes_requested')
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: CountIssueVerdictBreaksSince :one
-- Rows after a verdict that void it: a later CHANGES, a stale or superseded
-- mark, or a return to in_progress.
SELECT count(*)::bigint FROM activity_log
WHERE issue_id = sqlc.arg('issue_id') AND created_at > sqlc.arg('since')
  AND (action IN ('review_changes_requested', 'review_verdict_stale', 'review_superseded')
       OR (action = 'status_changed' AND details->>'to' = 'in_progress'));

-- name: CountIssueTextCandidateChangesSince :one
-- With no pull request the candidate is the issue's own text. A change after
-- the verdict is a new attachment, a description revision, or a comment that
-- was posted, edited or deleted. The reviewer's comments and the platform's
-- are not candidate text.
SELECT (
    (SELECT count(*) FROM attachment a
     WHERE a.issue_id = sqlc.arg('issue_id') AND a.created_at > sqlc.arg('since'))
  + (SELECT count(*) FROM issue_description_revision r
     WHERE r.issue_id = sqlc.arg('issue_id') AND r.created_at > sqlc.arg('since'))
  + (SELECT count(*) FROM comment c
     WHERE c.issue_id = sqlc.arg('issue_id')
       AND (c.created_at > sqlc.arg('since') OR c.updated_at > sqlc.arg('since') OR c.deleted_at > sqlc.arg('since'))
       AND c.author_type <> 'system'
       AND NOT (c.author_type = 'agent' AND c.author_id IS NOT DISTINCT FROM sqlc.narg('reviewer_id')::uuid))
)::bigint;

-- name: ListIssuePullRequestHeads :many
-- Every pull request linked to the issue, GitHub and self-hosted, with the head
-- it stands at (empty when the provider does not record one).
SELECT pr.state, pr.head_sha::text AS head_sha, pr.base_ref::text AS base_ref, pr.default_branch::text AS default_branch FROM github_pull_request pr
JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = $1
UNION ALL
SELECT pr.state, ''::text AS head_sha, ''::text AS base_ref, ''::text AS default_branch FROM vcs_pull_request pr
JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = $1;

-- name: CountIssueWakeupsLiveAtEntry :one
-- Entering a done-category step disables the issue's wakeups in the same
-- transaction, so "enabled" after the write is always empty. A wakeup that was
-- live at entry is one still enabled, or disabled at or after the newest move
-- into the step being entered (that move's own close rule). other leaves out
-- the issue's own workflow_edges watcher (CHE-7671): on an auto-close step it
-- is the merge wait, not work. own counts that watcher while it is enabled.
SELECT
    (count(*) FILTER (WHERE w.system_rule IS DISTINCT FROM 'workflow_edges'))::bigint AS other,
    (count(*) FILTER (WHERE w.system_rule = 'workflow_edges' AND w.enabled))::bigint AS own
FROM issue_wakeup w
WHERE w.issue_id = sqlc.arg('issue_id') AND (w.enabled OR w.disabled_at >= (
    SELECT max(a.created_at) FROM activity_log a
    WHERE a.issue_id = sqlc.arg('issue_id') AND a.action = 'status_changed' AND a.details->>'to' = sqlc.arg('step')::text));

-- name: GetNewestIssueRunByAgent :one
SELECT id FROM agent_task_queue
WHERE issue_id = $1 AND agent_id = $2
ORDER BY created_at DESC, id DESC
LIMIT 1;
