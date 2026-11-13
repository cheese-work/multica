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
UPDATE provider_breaker
SET state = 'closed', canary_task_id = NULL, opened_at = NULL, next_probe_at = NULL,
    cool_off_s = @default_cool_off_seconds, last_success_at = now(), version = version + 1
WHERE workspace_id = @workspace_id AND provider = @provider
  AND state = 'half_open' AND canary_task_id = @task_id AND version = @expected_version;

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
