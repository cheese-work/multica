-- name: LockWakeupAggregateBudget :one
-- Creates the budget on first use and holds its row lock to the end of the
-- transaction, so starts against one counter are admitted one at a time. The
-- stored limit follows the configuration resolved now.
INSERT INTO wakeup_aggregate_budget(workspace_id,scope_kind,scope_id,rule_key,starts_per_hour)
VALUES(@workspace_id,@scope_kind,@scope_id,@rule_key,@starts_per_hour)
ON CONFLICT (workspace_id,scope_kind,scope_id,rule_key)
DO UPDATE SET starts_per_hour=EXCLUDED.starts_per_hour,updated_at=clock_timestamp()
RETURNING starts_per_hour;

-- name: CountWakeupAggregateStarts :one
-- Paid starts of a counter inside the window, other than the asking task's own.
-- A run that was cancelled or failed before it ever started was never paid for
-- and a purged task is gone, so neither counts: no release step can be missed.
SELECT count(*) FROM wakeup_aggregate_reservation r JOIN agent_task_queue t ON t.id=r.task_id
WHERE r.workspace_id= @workspace_id AND r.scope_kind= @scope_kind AND r.scope_id= @scope_id AND r.rule_key= @rule_key
 AND r.reserved_at> @since AND r.task_id<> @task_id
 AND NOT (t.started_at IS NULL AND t.status IN ('cancelled','failed'));

-- name: WakeupAggregateFreeAt :one
-- When the (skip+1)th oldest counted start leaves the window: the first moment a
-- full counter has room again. skip is how far over the limit the count is.
SELECT r.reserved_at FROM wakeup_aggregate_reservation r JOIN agent_task_queue t ON t.id=r.task_id
WHERE r.workspace_id= @workspace_id AND r.scope_kind= @scope_kind AND r.scope_id= @scope_id AND r.rule_key= @rule_key
 AND r.reserved_at> @since AND r.task_id<> @task_id
 AND NOT (t.started_at IS NULL AND t.status IN ('cancelled','failed'))
ORDER BY r.reserved_at OFFSET @skip::int LIMIT 1;

-- name: ReserveWakeupAggregateStart :exec
-- One task counts once against one counter however often admission is retried.
INSERT INTO wakeup_aggregate_reservation(workspace_id,scope_kind,scope_id,rule_key,task_id,wakeup_id,reserved_at)
VALUES(@workspace_id,@scope_kind,@scope_id,@rule_key,@task_id,@wakeup_id,@reserved_at)
ON CONFLICT (workspace_id,scope_kind,scope_id,rule_key,task_id) DO NOTHING;

-- name: MarkWakeupAggregateBlocked :exec
-- A full counter delays the instance: it keeps its pending facts, names the
-- counter and says when the scheduler may look again.
UPDATE issue_wakeup SET aggregate_blocked_scope_kind= @scope_kind,aggregate_blocked_scope_id= @scope_id,aggregate_retry_at= @retry_at
WHERE id= @id;

-- name: ClearWakeupAggregateBlocked :exec
UPDATE issue_wakeup SET aggregate_blocked_scope_kind=NULL,aggregate_blocked_scope_id=NULL,aggregate_retry_at=NULL
WHERE id= @id AND aggregate_retry_at IS NOT NULL;

-- name: SummarizeWakeupAggregateBacklog :one
-- Instances a counter is holding back that still have facts waiting, and the
-- earliest time the scheduler will look at any of them again.
SELECT count(*)::int AS backlog,min(w.aggregate_retry_at)::timestamptz AS earliest_retry FROM issue_wakeup w
WHERE w.workspace_id= @workspace_id AND w.aggregate_blocked_scope_kind= @scope_kind AND w.aggregate_blocked_scope_id= @scope_id
 AND w.aggregate_retry_at IS NOT NULL AND COALESCE(w.system_rule,w.default_rule_key)= @rule_key::text
 AND EXISTS(SELECT 1 FROM issue_wakeup_receipt r WHERE r.wakeup_id=w.id AND r.processed_at IS NULL);
