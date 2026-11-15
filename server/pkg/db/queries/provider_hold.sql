-- name: ReleaseProviderHeldTask :one
-- The controller supplies its observed version. Lock and recheck the breaker
-- in this statement: a concurrent Closed -> Open update releases nothing.
-- Removing the hold does not promote the row or remove its media gate.
WITH breaker AS MATERIALIZED (
    SELECT b.workspace_id, b.provider FROM provider_breaker b
    WHERE b.workspace_id = @workspace_id AND b.provider = @provider
      AND b.state = 'closed' AND b.version = @expected_version
    FOR UPDATE
)
UPDATE agent_task_queue t
SET context = t.context - 'provider_hold',
    fire_at = CASE
        WHEN t.status = 'deferred' AND COALESCE(t.context->>'channel_issue_media_pending', '') <> 'true'
        THEN COALESCE(t.fire_at, now()) ELSE t.fire_at END
FROM breaker pb, agent a
WHERE t.id = @task_id AND t.issue_id IS NOT NULL
  AND a.id = t.agent_id AND a.workspace_id = pb.workspace_id
  AND t.status IN ('deferred', 'queued')
  AND t.context ? 'provider_hold'
  AND t.context->'provider_hold'->>'provider' = pb.provider
  AND (t.context->'provider_hold'->>'max_hold_until')::timestamptz > now()
RETURNING t.*;

-- name: FailExpiredProviderHolds :many
-- Fail and notify together. The inbox id is the task id, shared with existing
-- provider recovery notices, so a task can never notify its owner twice.
WITH victims AS (
    SELECT id FROM agent_task_queue
    WHERE issue_id IS NOT NULL AND status = 'deferred'
      AND context ? 'provider_hold'
      AND (context->'provider_hold'->>'max_hold_until')::timestamptz <= @now::timestamptz
    ORDER BY created_at, id
    LIMIT @max_per_tick::int
    FOR UPDATE SKIP LOCKED
), failed AS (
    UPDATE agent_task_queue t
    SET status = 'failed', completed_at = @now::timestamptz,
        failure_reason = 'provider_breaker_hold_expired',
        error = 'The provider breaker hold expired. Check the provider, then rerun the issue when it is healthy.',
        wait_reason = NULL, prepare_lease_expires_at = NULL
    FROM victims v
    WHERE t.id = v.id AND t.status = 'deferred' AND t.context ? 'provider_hold'
      AND (t.context->'provider_hold'->>'max_hold_until')::timestamptz <= @now::timestamptz
    RETURNING t.*
), notice AS (
    INSERT INTO inbox_item (
        id, workspace_id, recipient_type, recipient_id, type, severity,
        issue_id, title, body, actor_type, details
    )
    SELECT t.id, i.workspace_id, 'member', t.accountable_user_id,
        'provider_failure_recovery_blocked', 'action_required',
        t.issue_id, 'Provider recovery needs attention', t.error, 'system',
        jsonb_build_object('reason_code', t.failure_reason, 'failed_task_id', t.id)
    FROM failed t
    JOIN issue i ON i.id = t.issue_id
    JOIN member m ON m.workspace_id = i.workspace_id AND m.user_id = t.accountable_user_id
    ON CONFLICT (id) DO NOTHING
)
SELECT * FROM failed;
