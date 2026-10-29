-- PR event store (CHE-2298 a1).

-- name: LockGitHubPRForEvent :one
-- Serializes event capture per PR row and returns the current head and state.
SELECT id, workspace_id, head_sha, state
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
    workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key, status, previous_head_sha
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (issue_id, pull_request_id, dedupe_key) DO NOTHING;

-- name: GetGitHubPRSnapshotState :one
-- Previous snapshot verdicts, read before UpdateGitHubPRSnapshot overwrites them.
SELECT api_mergeable, checks_rollup_state, snapshot_head_sha
FROM github_pull_request
WHERE id = $1
FOR UPDATE;


-- name: ListNextTurnPREvents :many
-- Events no mid-run delivery will take (CHE-2298 a3): next_turn ones, and
-- accepted ones while the issue has no live run with the PR-event channel. Not
-- filtered by the capture switch: that stops new capture only, stored events
-- keep draining. An event with a receipt still in flight (pending, or carried by
-- a run that is queued, dispatched or has started) is skipped; a receipt that
-- was discarded or whose run ended unstarted leaves the event eligible again.
-- A started carrier is excluded here, not only swept to delivered, so a start
-- between the sweep and this read never makes the event eligible twice. The
-- after_* pair is the caller's cursor: a batch full of causes the rule cannot
-- take now must not hold every later batch back.
SELECT e.id, e.workspace_id, e.issue_id, e.kind, e.head_sha, e.base_sha, e.observed_at,
       pr.pr_number, pr.repo_owner, pr.repo_name,
       COALESCE(NULLIF(e.previous_head_sha, ''), (
           SELECT prev.head_sha FROM issue_pr_event prev
           WHERE e.kind = 'head_changed'
             AND prev.issue_id = e.issue_id
             AND prev.pull_request_id = e.pull_request_id
             AND prev.head_sha <> e.head_sha
             AND prev.status <> 'superseded'
             AND prev.observed_at < e.observed_at
           ORDER BY prev.observed_at DESC
           LIMIT 1
       ), '')::text AS old_head_sha,
       COALESCE((
           SELECT cr.name FROM github_pull_request_check_run cr
           WHERE e.kind = 'checks_failed'
             AND cr.pr_id = e.pull_request_id
             AND cr.head_sha = e.head_sha
             AND cr.conclusion IN ('failure', 'timed_out', 'cancelled', 'action_required', 'startup_failure')
           ORDER BY cr.ordinal
           LIMIT 1
       ), '')::text AS check_name
FROM issue_pr_event e
JOIN github_pull_request pr ON pr.id = e.pull_request_id
WHERE e.kind NOT IN ('draft', 'ready')
  AND (e.status = 'next_turn'
       OR (e.status = 'accepted'
           AND NOT EXISTS (
               SELECT 1 FROM agent_task_queue t
               JOIN task_pr_event_capability cap ON cap.task_id = t.id
               WHERE t.issue_id = e.issue_id AND t.status = 'running')))
  AND NOT EXISTS (
      SELECT 1
      FROM issue_wakeup w
      JOIN issue_wakeup_receipt r ON r.wakeup_id = w.id
      LEFT JOIN agent_task_queue t ON t.id = r.task_id
      WHERE w.issue_id = e.issue_id
        AND w.system_rule = 'pr_needs_attention'
        AND r.event_type = 'pr.event'
        AND r.payload->>'event_id' = e.id::text
        AND (r.processed_at IS NULL OR t.status IN ('queued', 'dispatched') OR t.started_at IS NOT NULL))
  AND (sqlc.narg('workspace_ids')::uuid[] IS NULL OR e.workspace_id = ANY(sqlc.narg('workspace_ids')::uuid[]))
  AND (sqlc.narg('after_observed_at')::timestamptz IS NULL
       OR (e.observed_at, e.id) > (sqlc.narg('after_observed_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY e.observed_at, e.id
LIMIT @row_limit
FOR UPDATE OF e SKIP LOCKED;

-- name: MarkPREventNextTurn :exec
-- The fallback owns the event from here: typed delivery takes accepted ones only.
UPDATE issue_pr_event SET status = 'next_turn' WHERE id = @id AND status = 'accepted';

-- name: MarkStartedPREventsDelivered :execrows
-- A next-turn wake that started carries its receipts' events.
UPDATE issue_pr_event e
SET status = 'delivered'
WHERE e.status = 'next_turn'
  AND (sqlc.narg('workspace_ids')::uuid[] IS NULL OR e.workspace_id = ANY(sqlc.narg('workspace_ids')::uuid[]))
  AND EXISTS (
      SELECT 1
      FROM issue_wakeup w
      JOIN issue_wakeup_receipt r ON r.wakeup_id = w.id
      JOIN agent_task_queue t ON t.id = r.task_id
      WHERE w.issue_id = e.issue_id
        AND w.system_rule = 'pr_needs_attention'
        AND r.event_type = 'pr.event'
        AND r.payload->>'event_id' = e.id::text
        AND t.started_at IS NOT NULL);

-- name: PREventQueuedNoted :one
SELECT EXISTS (
    SELECT 1 FROM activity_log
    WHERE issue_id = @issue_id AND details->>'event_id' = @event_id::text
)::boolean;
