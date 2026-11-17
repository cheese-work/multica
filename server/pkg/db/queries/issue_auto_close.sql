-- name: GetLatestIssueVerdict :one
-- CHE-6969: the verdict of record is the newest verdict row; its details carry
-- the findings, the reviewed pull request and the head it names.
SELECT action, details, created_at FROM activity_log
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

-- name: CountIssueAttachmentsSince :one
SELECT count(*)::bigint FROM attachment
WHERE issue_id = sqlc.arg('issue_id') AND created_at > sqlc.arg('since');

-- name: ListIssuePullRequestHeads :many
-- Every pull request linked to the issue, GitHub and self-hosted, with the head
-- it stands at (empty when the provider does not record one).
SELECT pr.state, pr.head_sha::text AS head_sha FROM github_pull_request pr
JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = $1
UNION ALL
SELECT pr.state, ''::text AS head_sha FROM vcs_pull_request pr
JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = $1;

-- name: CountIssueWakeupsLiveAtEntry :one
-- Entering a done-category step disables the issue's wakeups in the same
-- transaction, so "enabled" after the write is always empty. A wakeup that was
-- live at entry is one still enabled, or disabled at or after the newest move
-- into agent_accepted (that move's own close rule).
SELECT count(*)::bigint FROM issue_wakeup w
WHERE w.issue_id = $1 AND (w.enabled OR w.disabled_at >= (
    SELECT max(a.created_at) FROM activity_log a
    WHERE a.issue_id = $1 AND a.action = 'status_changed' AND a.details->>'to' = 'agent_accepted'));

-- name: GetNewestIssueRunByAgent :one
SELECT id FROM agent_task_queue
WHERE issue_id = $1 AND agent_id = $2
ORDER BY created_at DESC, id DESC
LIMIT 1;
