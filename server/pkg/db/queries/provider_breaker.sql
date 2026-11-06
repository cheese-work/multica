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

-- name: GetProviderBreakerVersion :one
SELECT version FROM provider_breaker WHERE workspace_id = @workspace_id AND provider = @provider;

-- name: GetProviderBreakerState :one
SELECT state FROM provider_breaker WHERE workspace_id = @workspace_id AND provider = @provider;

-- name: RecordProviderSuccess :execrows
INSERT INTO provider_breaker (workspace_id, provider, last_success_at)
VALUES (@workspace_id, @provider, now())
ON CONFLICT (workspace_id, provider) DO UPDATE
SET last_success_at = now(), version = provider_breaker.version + 1
WHERE provider_breaker.version = @expected_version;
