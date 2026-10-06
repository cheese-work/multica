-- name: ClaimWakeupScopedEvents :many
-- The oldest pending outbox inputs. SKIP LOCKED lets concurrent schedulers take
-- different rows; a row whose last attempt hit a real database error waits for
-- its retry time so it cannot starve the rest. An input captured before @oldest
-- is past retention: it is never claimed, so it can only expire, and neither the
-- size of an expired backlog nor the order of the prune can make it deliver.
-- @skip_issues are the issues this pass found busy. The scan starts after
-- (@after_at, @after_id), the drain's fair position, and stops at the sweep's
-- horizon (@until_at, @until_id): a pass moves the position past what it tried,
-- so the next pass reaches the work behind it, and a sweep ends at its horizon,
-- so inputs that arrive during it wait for the next sweep instead of extending
-- it, and every older input is reached again.
SELECT * FROM wakeup_scoped_event
WHERE handled_at IS NULL AND captured_at >= @oldest::timestamptz AND (retry_at IS NULL OR retry_at <= @now::timestamptz)
 AND (captured_at, id) > (@after_at::timestamptz, @after_id::uuid)
 AND (captured_at, id) <= (@until_at::timestamptz, @until_id::uuid)
 AND (sqlc.narg(workspace_ids)::uuid[] IS NULL OR workspace_id = ANY(sqlc.narg(workspace_ids)::uuid[]))
 AND issue_id <> ALL(COALESCE(@skip_issues::uuid[], '{}'::uuid[]))
ORDER BY captured_at, id LIMIT @batch_size
FOR UPDATE SKIP LOCKED;

-- name: NewestPendingWakeupScopedEvent :one
-- The newest pending input: the horizon of a new sweep.
SELECT captured_at,id FROM wakeup_scoped_event WHERE handled_at IS NULL ORDER BY captured_at DESC,id DESC LIMIT 1;

-- name: ClaimWakeupScopedEventsOfIssue :many
-- The rest of one issue's pending inputs, so a burst on one issue is resolved
-- once. Same locking and retry rules as the oldest-first claim.
SELECT * FROM wakeup_scoped_event
WHERE issue_id= @issue_id AND handled_at IS NULL AND captured_at >= @oldest::timestamptz AND (retry_at IS NULL OR retry_at <= @now::timestamptz) AND id<> @except_id
ORDER BY captured_at, id LIMIT @batch_size
FOR UPDATE SKIP LOCKED;

-- name: MarkWakeupScopedEventHandled :exec
UPDATE wakeup_scoped_event SET handled_at= @now::timestamptz,outcome= @outcome::text,retry_at=NULL WHERE id= @id;

-- name: DeferWakeupScopedEvent :exec
UPDATE wakeup_scoped_event SET retry_at= @retry_at::timestamptz WHERE id= @id AND handled_at IS NULL;

-- name: TryLockWakeupIssue :one
-- The drain's issue lock never waits: a held issue yields no row, and the caller
-- tells it from a deleted one with WakeupIssueExists.
SELECT * FROM issue WHERE id= @id FOR NO KEY UPDATE SKIP LOCKED;

-- name: WakeupIssueExists :one
SELECT EXISTS(SELECT 1 FROM issue WHERE id= @id);

-- name: ExpireWakeupScopedEvents :execrows
-- Pending inputs past retention are closed with a visible outcome, never
-- dropped silently and never promised. A row a drain holds is skipped, and the
-- outer statement rechecks that it is still pending, so an input delivered at
-- this moment keeps its outcome. The picked ids are materialized first: a plain
-- IN (subquery ... LIMIT) lets the planner run the subquery once per outer row,
-- which does not bound the batch.
WITH picked AS MATERIALIZED (
 SELECT e.id FROM wakeup_scoped_event e WHERE e.handled_at IS NULL AND e.captured_at < @before::timestamptz
 ORDER BY e.captured_at, e.id LIMIT @batch_size FOR UPDATE SKIP LOCKED)
UPDATE wakeup_scoped_event w SET handled_at= @now::timestamptz,outcome='expired',retry_at=NULL
FROM picked WHERE w.id=picked.id AND w.handled_at IS NULL;

-- name: DeleteHandledWakeupScopedEvents :execrows
-- Same materialized pick, for the same reason.
WITH picked AS MATERIALIZED (
 SELECT e.id FROM wakeup_scoped_event e WHERE e.handled_at IS NOT NULL AND e.handled_at < @before::timestamptz
 ORDER BY e.handled_at, e.id LIMIT @batch_size FOR UPDATE SKIP LOCKED)
DELETE FROM wakeup_scoped_event w USING picked WHERE w.id=picked.id;

-- name: CountWakeupScopedEventsByOutcome :many
-- Accounting of one workspace's retained inputs; an input not yet handled counts as pending.
SELECT COALESCE(outcome,'pending')::text AS outcome,count(*) AS total FROM wakeup_scoped_event WHERE workspace_id= @workspace_id GROUP BY 1;

-- name: ReplayScopedWakeupEvent :exec
-- Delivers one captured event into the named instances only, through the same
-- receipt, coalescing and idempotency rules as live capture. The captured time
-- and actor are the event's own, not the drain's.
SELECT capture_issue_wakeup(@issue_id::uuid,@event_type::text,@event_key::text,sqlc.narg(agent_id)::uuid,sqlc.narg(source_task_id)::uuid,
 @payload::jsonb || jsonb_build_object('occurred_at',@captured_at::timestamptz,'actor_type',sqlc.narg(actor_type)::text,'actor_id',sqlc.narg(actor_id)::text),
 @instance_ids::uuid[]);

-- name: GetDefaultWakeupInstance :one
-- The issue's runtime instance of one default rule; the issue row lock held by
-- the caller serializes creation.
SELECT * FROM issue_wakeup WHERE issue_id= @issue_id AND default_rule_key= @rule_key::text AND system_rule IS NULL ORDER BY id LIMIT 1;

-- name: CreateDefaultWakeupInstance :one
-- Created disabled: the capacity guard decides on enabling it.
INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,created_by,instruction,kind,mode,event_types,max_fires,enabled,default_rule_key,default_scope_kind,default_scope_id,config_fingerprint)
VALUES(@id,@workspace_id,@issue_id,@agent_id,@created_by,@instruction,'event',@mode,@event_types,sqlc.narg(max_fires),false,@rule_key,@scope_kind,@scope_id,@fingerprint)
RETURNING *;

-- name: RebaseDefaultWakeupInstance :one
-- The instance moves to the configuration it now resolves to. The revision
-- moves with it, so inputs captured under the old configuration stop matching;
-- identity, fire count, pauses and consumed state stay as they are.
UPDATE issue_wakeup SET agent_id= @agent_id,created_by= @created_by,instruction= @instruction,mode= @mode,event_types= @event_types,max_fires=sqlc.narg(max_fires),
 config_fingerprint= @fingerprint,revision=revision+1,updated_at=clock_timestamp()
WHERE id= @id AND default_rule_key IS NOT NULL AND system_rule IS NULL
RETURNING *;
