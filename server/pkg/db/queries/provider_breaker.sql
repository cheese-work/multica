-- name: LockProviderBreakerTrip :exec
SELECT pg_advisory_xact_lock(hashtextextended(@lock_key::text, 0));

-- name: ListRecentProviderServerFailures :many
SELECT t.id, a.model, COALESCE(t.completed_at, t.created_at) AS failed_at
FROM agent_task_queue t
JOIN agent a ON a.id = t.agent_id
WHERE a.workspace_id = @workspace_id
  AND t.status = 'failed'
  AND t.failure_reason = 'agent_error.provider_server_error'
  AND COALESCE(t.completed_at, t.created_at) >= now() - make_interval(secs => @window_seconds::int)
ORDER BY COALESCE(t.completed_at, t.created_at) DESC;

-- name: OpenProviderBreaker :execrows
INSERT INTO provider_breaker (workspace_id, provider, state, opened_at, next_probe_at, cool_off_s, trip_evidence, last_5xx_at)
VALUES (@workspace_id, @provider, 'open', now(), now() + make_interval(secs => @cool_off_seconds::int), @cool_off_seconds, @trip_evidence, now())
ON CONFLICT (workspace_id, provider) DO UPDATE SET
    state = CASE WHEN provider_breaker.state = 'closed' THEN 'open' ELSE provider_breaker.state END,
    opened_at = CASE WHEN provider_breaker.state = 'closed' THEN now() ELSE provider_breaker.opened_at END,
    next_probe_at = CASE WHEN provider_breaker.state = 'closed' THEN now() + make_interval(secs => provider_breaker.cool_off_s) ELSE provider_breaker.next_probe_at END,
    trip_evidence = CASE WHEN provider_breaker.state = 'closed' THEN EXCLUDED.trip_evidence ELSE provider_breaker.trip_evidence END,
    last_5xx_at = now(), version = provider_breaker.version + 1
WHERE provider_breaker.version = @expected_version;

-- name: GetProviderBreaker :one
-- probe_due uses the database clock: NULL next_probe_at counts as due so an
-- open breaker can never wait without a bound. A canary task that has not
-- reached a terminal status and is in flight (dispatched, running,
-- waiting_local_directory) is never replaced, even past its deadline: it is
-- fenced until it settles, so its own late result still lands. A queued or
-- deferred canary is not in flight (claims are held while half_open, so it
-- cannot start), so the deadline alone bounds it.
SELECT pb.*, (
    (pb.next_probe_at IS NULL OR pb.next_probe_at <= now())
    AND NOT EXISTS (
        SELECT 1 FROM agent_task_queue t
        WHERE t.id = pb.canary_task_id AND t.status IN ('dispatched', 'running', 'waiting_local_directory'))
)::bool AS probe_due
FROM provider_breaker pb WHERE pb.workspace_id = @workspace_id AND pb.provider = @provider;

-- name: AcquireProviderBreakerCanary :execrows
-- Moves a due open (or deadline-expired half_open) breaker to half_open bound
-- to one task. next_probe_at becomes the canary deadline; if the canary settles
-- without reporting, the breaker is due again after it. A canary that has
-- not settled blocks replacement (same in-flight fence as probe_due,
-- except the task being claimed: re-claiming the bound canary itself is allowed).
UPDATE provider_breaker
SET state = 'half_open', canary_task_id = @task_id,
    next_probe_at = now() + make_interval(secs => @canary_deadline_seconds::int),
    version = version + 1
WHERE workspace_id = @workspace_id AND provider = @provider
  AND state IN ('open', 'half_open')
  AND (next_probe_at IS NULL OR next_probe_at <= now())
  AND NOT EXISTS (
      SELECT 1 FROM agent_task_queue t
      WHERE t.id = provider_breaker.canary_task_id AND t.id <> @task_id
        AND t.status IN ('dispatched', 'running', 'waiting_local_directory'));

-- name: CloseProviderBreakerOnCanary :execrows
-- A canary success closes the breaker, or starts the drain (CHE-1741 R3) when
-- provider-held rows are waiting: draining releases that backlog staggered and
-- keeps later arrivals held until it is empty.
UPDATE provider_breaker
SET state = CASE WHEN EXISTS (
        SELECT 1 FROM agent_task_queue h JOIN agent a ON a.id = h.agent_id
        WHERE a.workspace_id = provider_breaker.workspace_id AND h.status = 'deferred'
          AND h.context->'provider_hold'->>'provider' = provider_breaker.provider
    ) THEN 'draining' ELSE 'closed' END,
    drain_cutoff_at = now(), canary_task_id = NULL, opened_at = NULL, next_probe_at = NULL,
    cool_off_s = @default_cool_off_seconds, canary_cool_off_s = @default_cool_off_seconds,
    next_canary_at = NULL, last_success_at = now(), version = version + 1
WHERE provider_breaker.workspace_id = @workspace_id AND provider_breaker.provider = @provider
  AND provider_breaker.state = 'half_open' AND provider_breaker.canary_task_id = @task_id
  AND provider_breaker.version = @expected_version;

-- name: ReopenProviderBreaker :execrows
UPDATE provider_breaker
SET state = 'open', canary_task_id = NULL, opened_at = now(),
    cool_off_s = LEAST(cool_off_s * 2, @max_cool_off_seconds::int),
    next_probe_at = now() + make_interval(secs => LEAST(cool_off_s * 2, @max_cool_off_seconds::int)),
    last_5xx_at = now(), version = version + 1
WHERE workspace_id = @workspace_id AND provider = @provider
  AND state = 'half_open' AND canary_task_id = @task_id AND version = @expected_version;

-- name: RecordProviderSuccess :execrows
INSERT INTO provider_breaker (workspace_id, provider, last_success_at)
VALUES (@workspace_id, @provider, now())
ON CONFLICT (workspace_id, provider) DO UPDATE
SET last_success_at = now(), version = provider_breaker.version + 1
WHERE provider_breaker.version = @expected_version;

-- name: ListProviderBreakers :many
SELECT * FROM provider_breaker WHERE workspace_id = @workspace_id ORDER BY provider;

-- name: ResetProviderBreaker :execrows
UPDATE provider_breaker
SET state = 'closed', canary_task_id = NULL, opened_at = NULL, next_probe_at = NULL,
    cool_off_s = @default_cool_off_seconds, version = version + 1
WHERE workspace_id = @workspace_id AND provider = @provider;

-- name: InsertProviderBreakerAudit :exec
INSERT INTO provider_breaker_audit (workspace_id, provider, event, previous_state, new_state, actor_id, task_id)
VALUES (@workspace_id, @provider, @event, @previous_state, @new_state, @actor_id, @task_id);

-- name: ListProviderHeldGroups :many
-- Groups with held rows, plus draining breakers: a drain with no rows left must
-- still be visited so it can close.
SELECT a.workspace_id, (t.context->'provider_hold'->>'provider')::text AS provider
FROM agent_task_queue t JOIN agent a ON a.id = t.agent_id
WHERE t.status = 'deferred' AND t.issue_id IS NOT NULL AND t.context ? 'provider_hold'
UNION
SELECT workspace_id, provider FROM provider_breaker WHERE state = 'draining';

-- name: ListProviderHeldRows :many
-- Oldest first. failed_at is the completion of the task this row retries; NULL
-- when there is none. Rows past their hold deadline are the expiry sweeper's.
SELECT t.id, (t.context->'provider_hold'->>'since')::timestamptz AS since,
       p.completed_at AS failed_at,
       EXISTS (SELECT 1 FROM provider_failure_recovery r WHERE r.recovery_task_id = t.id)::bool AS is_recovery
FROM agent_task_queue t
JOIN agent a ON a.id = t.agent_id
LEFT JOIN agent_task_queue p ON p.id = t.retry_of_task_id
WHERE a.workspace_id = @workspace_id AND t.status = 'deferred' AND t.issue_id IS NOT NULL
  AND t.context->'provider_hold'->>'provider' = @provider::text
  AND (t.context->'provider_hold'->>'max_hold_until')::timestamptz > @now::timestamptz
ORDER BY (t.context->'provider_hold'->>'since')::timestamptz, t.id
LIMIT @max_rows::int;

-- name: ReleaseProviderHeldRow :one
-- Removes the hold, or cancels the row as wakeup_superseded when it is bound to
-- a wakeup that is now disabled or at another revision, or the workspace is
-- paused. A locked or no longer held row returns no rows.
WITH target AS MATERIALIZED (
    SELECT t.id, (t.context->>'wakeup_id' IS NULL OR (
        EXISTS (SELECT 1 FROM issue_wakeup w WHERE w.id = (t.context->>'wakeup_id')::uuid
                AND w.disabled_at IS NULL AND w.revision = (t.context->>'wakeup_revision')::bigint)
        AND NOT EXISTS (SELECT 1 FROM workspace ws WHERE ws.id = @workspace_id AND ws.scheduling_paused_at IS NOT NULL)
    )) AS live
    FROM agent_task_queue t JOIN agent a ON a.id = t.agent_id
    WHERE t.id = @task_id AND a.workspace_id = @workspace_id AND t.issue_id IS NOT NULL
      AND t.status = 'deferred' AND t.context ? 'provider_hold'
      AND t.context->'provider_hold'->>'provider' = @provider::text
      AND (t.context->'provider_hold'->>'max_hold_until')::timestamptz > @now::timestamptz
    FOR UPDATE OF t SKIP LOCKED
)
UPDATE agent_task_queue t
SET context = t.context - 'provider_hold',
    status = CASE WHEN target.live THEN t.status ELSE 'cancelled' END,
    failure_reason = CASE WHEN target.live THEN t.failure_reason ELSE 'wakeup_superseded' END,
    completed_at = CASE WHEN target.live THEN t.completed_at ELSE @now::timestamptz END,
    fire_at = CASE WHEN target.live AND COALESCE(t.context->>'channel_issue_media_pending', '') <> 'true'
                   THEN COALESCE(t.fire_at, @now::timestamptz) ELSE t.fire_at END
FROM target
WHERE t.id = target.id
RETURNING t.*;

-- name: BeginProviderBreakerProbe :execrows
-- open (or a half_open whose canary ended without a verdict) -> half_open bound
-- to the released row. next_probe_at = now() keeps the claim gate open for it.
UPDATE provider_breaker
SET state = 'half_open', canary_task_id = @task_id, opened_at = COALESCE(opened_at, now()),
    next_probe_at = now(), version = version + 1
WHERE workspace_id = @workspace_id AND provider = @provider
  AND state IN ('open', 'half_open') AND version = @expected_version;

-- name: SaveProviderReleaseState :execrows
-- Controller bookkeeping on a closed breaker: the canary slot, its cool-off and
-- start log (R2) and the stagger clock (R1).
UPDATE provider_breaker
SET canary_task_id = sqlc.narg('canary_task_id'), canary_cool_off_s = @canary_cool_off_s,
    next_canary_at = sqlc.narg('next_canary_at'), canary_starts = @canary_starts::timestamptz[],
    last_release_at = sqlc.narg('last_release_at'), version = version + 1
WHERE workspace_id = @workspace_id AND provider = @provider
  AND state = 'closed' AND version = @expected_version;

-- name: MarkProviderReleaseTick :execrows
-- Draining: records the stagger clock only.
UPDATE provider_breaker SET last_release_at = @now::timestamptz, version = version + 1
WHERE workspace_id = @workspace_id AND provider = @provider
  AND state = 'draining' AND version = @expected_version;

-- name: CloseDrainedProviderBreaker :execrows
UPDATE provider_breaker
SET state = 'closed', drain_cutoff_at = NULL, canary_task_id = NULL, next_canary_at = NULL,
    canary_cool_off_s = @default_cool_off_seconds, version = version + 1
WHERE workspace_id = @workspace_id AND provider = @provider
  AND state = 'draining' AND version = @expected_version;
