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
