package db

// CHE-9177: the query as it stood on main before the rewrite (parity oracle and
// negative control for the plan test).
// Params: $1 workspace_ids, $2 after_observed_at, $3 after_id, $4 row_limit.
const oldListNextTurnPREvents = `-- name: ListNextTurnPREvents :many
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
WHERE e.kind NOT IN ('draft', 'ready', 'merged')
  AND NOT (e.kind IN ('conflict', 'head_changed', 'checks_failed') AND pr.state IN ('merged', 'closed'))
  AND (e.status = 'next_turn'
       -- A member was told once (notified); an agent or squad holding the issue
       -- takes the event now (CHE-5901).
       OR (e.status = 'notified'
           AND EXISTS (SELECT 1 FROM issue i WHERE i.id = e.issue_id AND i.assignee_type IN ('agent', 'squad')))
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
  AND NOT EXISTS (
      SELECT 1
      FROM issue_wakeup lw
      JOIN issue_wakeup_receipt lr ON lr.wakeup_id = lw.id
      LEFT JOIN agent_task_queue lt ON lt.id = lr.task_id
      WHERE lw.issue_id = e.issue_id
        AND lr.payload->>'head_sha' = e.head_sha
        AND lr.payload->>'repo_owner' = pr.repo_owner
        AND lr.payload->>'repo_name' = pr.repo_name
        AND lr.payload->>'pr_number' = pr.pr_number::text
        AND ((e.kind = 'checks_failed' AND lw.system_rule = 'pr_checks_failed' AND lr.event_type = 'pr.checks_failed')
             OR (e.kind = 'conflict' AND lw.system_rule = 'pr_needs_attention' AND lr.event_type = 'pr.mergeability'
                 AND lr.payload->>'merge_state' = 'DIRTY' AND COALESCE(lr.payload->>'base_sha', '') = e.base_sha))
        AND (lr.processed_at IS NULL OR lt.status IN ('queued', 'dispatched') OR lt.started_at IS NOT NULL))
  AND ($1::uuid[] IS NULL OR e.workspace_id = ANY($1::uuid[]))
  AND ($2::timestamptz IS NULL
       OR (e.observed_at, e.id) > ($2::timestamptz, $3::uuid))
ORDER BY e.observed_at, e.id
LIMIT $4
FOR UPDATE OF e SKIP LOCKED
`
