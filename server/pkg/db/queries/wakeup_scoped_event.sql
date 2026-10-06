-- name: ClaimWakeupScopedEvents :many
-- The oldest pending outbox inputs. SKIP LOCKED lets concurrent schedulers take
-- different rows; a row whose last attempt hit a real database error waits for
-- its retry time so it cannot starve the rest. An input captured before @oldest
-- is past retention: it is never claimed, so it can only expire, and neither the
-- size of an expired backlog nor the order of the prune can make it deliver.
-- @skip_issues are the issues this pass found busy.
SELECT * FROM wakeup_scoped_event
WHERE handled_at IS NULL AND captured_at >= @oldest::timestamptz AND (retry_at IS NULL OR retry_at <= @now::timestamptz)
 AND (sqlc.narg(workspace_ids)::uuid[] IS NULL OR workspace_id = ANY(sqlc.narg(workspace_ids)::uuid[]))
 AND issue_id <> ALL(@skip_issues::uuid[])
ORDER BY captured_at, id LIMIT @batch_size
FOR UPDATE SKIP LOCKED;

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

-- name: DeferWakeupScopedEventsOfIssue :exec
-- A busy issue's pending inputs wait for @retry_at, so the next claim passes over
-- them and goes on to other work. It never shortens a wait, and a row another
-- scheduler holds is left to that scheduler. Retention is unaffected: the claims
-- still refuse an input captured before the retention cut.
UPDATE wakeup_scoped_event SET retry_at= @retry_at::timestamptz
WHERE id IN (SELECT e.id FROM wakeup_scoped_event e WHERE e.issue_id= @issue_id AND e.handled_at IS NULL AND (e.retry_at IS NULL OR e.retry_at < @retry_at::timestamptz) FOR UPDATE SKIP LOCKED);

-- name: ExpireWakeupScopedEvents :execrows
-- Pending inputs past retention are closed with a visible outcome, never
-- dropped silently and never promised. A row a drain holds is skipped, and the
-- outer statement rechecks that it is still pending, so an input delivered at
-- this moment keeps its outcome.
UPDATE wakeup_scoped_event SET handled_at= @now::timestamptz,outcome='expired',retry_at=NULL
WHERE handled_at IS NULL AND id IN (SELECT e.id FROM wakeup_scoped_event e WHERE e.handled_at IS NULL AND e.captured_at < @before::timestamptz ORDER BY e.captured_at LIMIT @batch_size FOR UPDATE SKIP LOCKED);

-- name: DeleteHandledWakeupScopedEvents :execrows
DELETE FROM wakeup_scoped_event
WHERE id IN (SELECT e.id FROM wakeup_scoped_event e WHERE e.handled_at IS NOT NULL AND e.handled_at < @before::timestamptz ORDER BY e.handled_at LIMIT @batch_size FOR UPDATE SKIP LOCKED);

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
-- Created disabled: the capacity guard decides on enabling it. A condition rule's
-- instance carries its predicate and the facts it starts from (condition_state);
-- it is evaluated by the scheduler from next_fire_at.
INSERT INTO issue_wakeup(id,workspace_id,issue_id,agent_id,created_by,instruction,kind,mode,event_types,max_fires,enabled,default_rule_key,default_scope_kind,default_scope_id,config_fingerprint,
 condition,condition_state,next_fire_at,expires_at)
VALUES(@id,@workspace_id,@issue_id,@agent_id,@created_by,@instruction,'event',@mode,@event_types,sqlc.narg(max_fires),false,@rule_key,@scope_kind,@scope_id,@fingerprint,
 sqlc.narg(condition)::jsonb,@condition_state::text,sqlc.narg(next_fire_at)::timestamptz,sqlc.narg(expires_at)::timestamptz)
RETURNING *;

-- name: RebaseDefaultWakeupInstance :one
-- The instance moves to the configuration it now resolves to. The revision
-- moves with it, so inputs captured under the old configuration stop matching;
-- identity, fire count, pauses and consumed state stay as they are. An
-- aggregate delay belonged to the old configuration's cap, so it ends too.
UPDATE issue_wakeup SET agent_id= @agent_id,created_by= @created_by,instruction= @instruction,mode= @mode,event_types= @event_types,max_fires=sqlc.narg(max_fires),
 condition=sqlc.narg(condition)::jsonb,condition_state= @condition_state::text,next_fire_at=sqlc.narg(next_fire_at)::timestamptz,expires_at=sqlc.narg(expires_at)::timestamptz,
 config_fingerprint= @fingerprint,revision=revision+1,updated_at=clock_timestamp(),
 aggregate_blocked_scope_kind=NULL,aggregate_blocked_scope_id=NULL,aggregate_retry_at=NULL
WHERE id= @id AND default_rule_key IS NOT NULL AND system_rule IS NULL
RETURNING *;
