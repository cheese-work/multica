-- name: ListStepWatchdogCandidates :many
-- CHE-6116 S1. Issues an agent or squad holds in a step, with no run, since
-- before older_than. The age is the newest status_changed row, never
-- updated_at: a comment must not reset the clock. occurrence_key names that
-- status occurrence (the activity row id, or created:<issue id> when the issue
-- has no status history), so it is never null. Whether the step has a handler
-- is decided in Go, from the workflow.
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
  AND i.status NOT IN ('backlog', 'merged', 'done', 'cancelled')
  AND NOT EXISTS (
    SELECT 1 FROM issue_status s
    WHERE s.workspace_id = i.workspace_id AND s.key = i.status AND s.category IN ('done', 'closed'))
  AND COALESCE(chg.act_at, i.created_at) < @older_than::timestamptz
  AND NOT EXISTS (
    SELECT 1 FROM agent_task_queue t
    WHERE t.issue_id = i.id AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred'))
  AND COALESCE(i.description, '') NOT LIKE 'Blockers: Cheese stop%'
  AND NOT EXISTS (
    SELECT 1 FROM issue p
    WHERE p.id = i.parent_issue_id AND p.metadata->>'delivery_stop' = 'stopped')
  AND NOT (i.status = 'need_decision' AND EXISTS (
    SELECT 1 FROM decision_request d
    WHERE d.workspace_id = i.workspace_id AND d.issue_id = i.id AND d.status = 'open'))
ORDER BY COALESCE(chg.act_at, i.created_at), i.id
LIMIT @row_limit::int;
