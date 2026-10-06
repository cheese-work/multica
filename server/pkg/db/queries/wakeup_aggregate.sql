-- name: LockWakeupAggregateBudget :one
-- Creates the budget on first use and holds its row lock to the end of the
-- transaction, so starts against one counter are admitted one at a time. The
-- stored limit follows the configuration resolved now.
INSERT INTO wakeup_aggregate_budget(workspace_id,scope_kind,scope_id,rule_key,starts_per_hour)
VALUES(@workspace_id,@scope_kind,@scope_id,@rule_key,@starts_per_hour)
ON CONFLICT (workspace_id,scope_kind,scope_id,rule_key)
DO UPDATE SET starts_per_hour=EXCLUDED.starts_per_hour,updated_at=clock_timestamp()
RETURNING starts_per_hour;

-- name: SummarizeWakeupAggregateStarts :one
-- Paid starts of a counter inside the window, other than the asking task's own,
-- and the moment it next has room: the (used-limit+1)th oldest counted start
-- leaving the window. One statement, so the two always describe the same set
-- even while runs are cancelled concurrently. A run that was cancelled or failed
-- before it ever started was never paid for and a purged task is gone, so
-- neither counts: no release step can be missed.
WITH counted AS (
 SELECT r.reserved_at,row_number() OVER (ORDER BY r.reserved_at) AS n
 FROM wakeup_aggregate_reservation r JOIN agent_task_queue t ON t.id=r.task_id
 WHERE r.workspace_id= @workspace_id AND r.scope_kind= @scope_kind AND r.scope_id= @scope_id AND r.rule_key= @rule_key
  AND r.reserved_at> @since AND r.task_id<> @task_id
  AND NOT (t.started_at IS NULL AND t.status IN ('cancelled','failed'))
)
SELECT (SELECT count(*) FROM counted)::bigint AS used,
 (SELECT c.reserved_at FROM counted c WHERE c.n=(SELECT count(*) FROM counted)- sqlc.arg(starts_per_hour)::bigint+1)::timestamptz AS free_at;

-- name: CountOlderWakeupAggregateWaiters :one
-- Instances this counter is already holding back whose oldest pending fact is
-- older than the asking instance's (since). Age is the age of the facts still
-- pending, so facts consumed without a start take their place in line with them,
-- and an instance with nothing pending is not waiting. They are first in line for
-- any room the counter has. A waiter whose retry time is older than fresh_after
-- has stopped being retried (its rule is held, say) and keeps nobody else waiting.
SELECT count(*) FROM issue_wakeup w
WHERE w.workspace_id= @workspace_id AND w.aggregate_blocked_scope_kind= @scope_kind AND w.aggregate_blocked_scope_id= @scope_id
 AND w.aggregate_retry_at> @fresh_after AND COALESCE(w.system_rule,w.default_rule_key)= @rule_key::text
 AND w.id<> @wakeup_id
 AND ((SELECT min(r.created_at) FROM issue_wakeup_receipt r WHERE r.wakeup_id=w.id AND r.processed_at IS NULL),w.id)<(sqlc.arg(since)::timestamptz, @wakeup_id::uuid);

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

-- name: PruneWakeupAggregateReservations :execrows
-- Retention: reservations far older than the counting window (and those of tasks
-- long purged, which are older still) no longer count. A bounded batch per pass,
-- oldest first, so the sweep never holds a long lock.
DELETE FROM wakeup_aggregate_reservation r USING (
 SELECT x.workspace_id,x.scope_kind,x.scope_id,x.rule_key,x.task_id FROM wakeup_aggregate_reservation x
 WHERE x.reserved_at< @before ORDER BY x.reserved_at LIMIT @batch::int
) d
WHERE r.workspace_id=d.workspace_id AND r.scope_kind=d.scope_kind AND r.scope_id=d.scope_id AND r.rule_key=d.rule_key AND r.task_id=d.task_id;
