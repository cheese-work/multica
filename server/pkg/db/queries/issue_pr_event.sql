-- PR event store (CHE-2298 a1).

-- name: LockGitHubPRForEvent :one
-- Serializes event capture per PR row and returns the current head.
SELECT id, workspace_id, head_sha
FROM github_pull_request
WHERE id = $1
FOR UPDATE;

-- name: ListIssueIDsForPR :many
SELECT issue_id FROM issue_pull_request WHERE pull_request_id = $1;

-- name: LockIssueForPREvent :one
-- Key-share-locks the issue so a concurrent DeleteIssue waits for this capture
-- instead of sweeping events before the capture's insert commits. No row means
-- the issue is already gone; the capture must write nothing for it.
-- NOWAIT: capture already holds the PR row lock, and workspace teardown takes
-- issue locks before PR locks. Waiting here would close that cycle (40P01), so a
-- held issue lock fails the capture (55P03); the snapshot tx rolls back whole and
-- the next refresh re-derives the same transition.
SELECT id FROM issue WHERE id = $1 FOR KEY SHARE NOWAIT;

-- name: DeleteIssuePREvents :exec
-- Second-pass sweep, run after DeleteIssue: it sees events a capture committed
-- while DeleteIssue waited on that capture's issue lock.
DELETE FROM issue_pr_event WHERE issue_id = $1 AND workspace_id = $2;

-- name: PREventKindEnabled :one
-- Absent or non-false = enabled; only an explicit false disables one kind.
SELECT COALESCE(
    (w.settings -> 'pr_event_capture' ->> sqlc.arg('kind')::text) IS DISTINCT FROM 'false',
    TRUE
)::boolean AS enabled
FROM workspace w
WHERE w.id = $1;

-- name: GetLatestIssuePREventObservedAt :one
-- Per-head watermark: head order outranks time, so an observation of another
-- head must never mark a current-head event as older.
SELECT max(observed_at)::timestamptz AS observed_at
FROM issue_pr_event
WHERE issue_id = $1 AND pull_request_id = $2 AND head_sha = $3;

-- name: InsertIssuePREvent :execrows
INSERT INTO issue_pr_event (
    workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key, status
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (issue_id, pull_request_id, dedupe_key) DO NOTHING;

-- name: GetGitHubPRSnapshotState :one
-- Previous snapshot verdicts, read before UpdateGitHubPRSnapshot overwrites them.
SELECT api_mergeable, checks_rollup_state, snapshot_head_sha
FROM github_pull_request
WHERE id = $1
FOR UPDATE;

