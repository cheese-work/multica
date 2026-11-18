-- name: ListStepWatchdogCandidates :many
-- CHE-6116 S1. Issues an agent or squad holds in a step, with no run, since
-- before older_than. The age is the newest status_changed row, never
-- updated_at: a comment must not reset the clock. occurrence_key names that
-- status occurrence (the activity row id, or created:<issue id> when the issue
-- has no status history), so it is never null. Whether the step has a handler
-- is decided in Go, from the workflow, so the caller pages with the (after_at,
-- after_id) keyset until it has enough candidates. A need_decision issue with
-- any decision request is the decision worker's: an open request is the ask, and
-- the wait-episode delivery owns answered, expired, withdrawn and cancelled ones.
-- ponytail: "any request" ignores age, so a stale request from an earlier
-- need_decision occurrence also hides the issue; scope it to the occurrence if seen.
SELECT i.id,
       COALESCE(chg.act_key, 'created:' || i.id::text)::text AS occurrence_key,
       COALESCE(chg.act_at, i.created_at)::timestamptz AS in_step_since
FROM issue i
LEFT JOIN LATERAL (
  SELECT a.id::text AS act_key, a.created_at AS act_at
  FROM activity_log a
  WHERE a.issue_id = i.id AND a.action = 'status_changed'
  ORDER BY a.created_at DESC, a.id DESC
  LIMIT 1
) chg ON true
WHERE i.assignee_type IN ('agent', 'squad')
  AND (i.project_id IS NOT NULL OR i.workflow_id IS NOT NULL)
  AND i.status NOT IN ('backlog', 'done', 'cancelled')
  AND NOT EXISTS (
    SELECT 1 FROM issue_status s
    WHERE s.workspace_id = i.workspace_id AND s.key = i.status AND s.category IN ('done', 'closed'))
  AND COALESCE(chg.act_at, i.created_at) < @older_than::timestamptz
  AND (COALESCE(chg.act_at, i.created_at), i.id) > (@after_at::timestamptz, @after_id::uuid)
  AND NOT EXISTS (
    SELECT 1 FROM agent_task_queue t
    WHERE t.issue_id = i.id AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred'))
  AND COALESCE(i.description, '') NOT LIKE 'Blockers: Cheese stop%'
  AND NOT EXISTS (
    SELECT 1 FROM issue p
    WHERE p.id = i.parent_issue_id AND p.metadata->>'delivery_stop' = 'stopped')
  AND NOT (i.status = 'need_decision' AND EXISTS (
    SELECT 1 FROM decision_request d
    WHERE d.workspace_id = i.workspace_id AND d.issue_id = i.id))
ORDER BY COALESCE(chg.act_at, i.created_at), i.id
LIMIT @row_limit::int;
