-- name: MarkBudgetRecoveryTask :exec
-- Tags the run the exhaustion move enqueued (CHE-2297 L3). The tag is what the
-- server keys the recovery limits on: the task-token restrictions and the
-- wall-clock cancel.
UPDATE agent_task_queue SET task_kind = 'budget_recovery' WHERE id = $1;

-- name: IsBudgetRecoveryTask :one
SELECT EXISTS (SELECT 1 FROM agent_task_queue WHERE id = $1 AND task_kind = 'budget_recovery');

-- name: ListExpiredBudgetRecoveryTasks :many
-- Recovery runs still live past their wall-clock limit.
SELECT id FROM agent_task_queue
WHERE task_kind = 'budget_recovery'
  AND status IN ('queued', 'dispatched', 'running')
  AND created_at < now() - make_interval(secs => sqlc.arg('limit_seconds')::float8);

-- name: ListRecentIssueRunResults :many
-- The issue's newest finished runs, so the exhaustion notice can show what the
-- last few runs did. A queued or running task has no result yet.
SELECT a.name AS agent_name, t.status, COALESCE(t.error, '')::text AS error,
       COALESCE(t.result->>'output', '')::text AS output
FROM agent_task_queue t
JOIN agent a ON a.id = t.agent_id
WHERE t.issue_id = sqlc.arg('issue_id')::uuid AND t.status IN ('completed', 'failed', 'cancelled')
ORDER BY t.created_at DESC, t.id DESC
LIMIT sqlc.arg('max_runs')::int;
