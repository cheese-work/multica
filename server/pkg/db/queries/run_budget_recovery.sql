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

-- name: DropRecoveryWakeupReceipts :exec
-- A comment a recovery run wrote starts no run through an event wakeup (CHE-2297
-- L3). The scheduler has no request to carry the mark, so the comment's source
-- task does. A receipt whose newest comment is a member's stays.
UPDATE issue_wakeup_receipt r SET processed_at = now()
WHERE r.wakeup_id = @wakeup_id AND r.revision = @revision AND r.processed_at IS NULL
  AND r.event_type = 'comment.created'
  AND EXISTS (
    SELECT 1 FROM comment c JOIN agent_task_queue t ON t.id = c.source_task_id
    WHERE c.id::text = r.payload->>'comment_id' AND t.task_kind = 'budget_recovery');
