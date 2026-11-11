-- Platform writes that carry a stable event id (CHE-5213). A redelivered write
-- replays the stored response instead of writing again.

-- name: ClaimWriteEvent :one
INSERT INTO write_event (workspace_id, event_key) VALUES ($1, $2)
ON CONFLICT (workspace_id, event_key) DO NOTHING
RETURNING *;

-- name: GetWriteEvent :one
SELECT * FROM write_event WHERE workspace_id = $1 AND event_key = $2;

-- name: RetakeStaleWriteEvent :one
-- A write that never finished (crash between claim and completion) can be taken
-- again once its claim is older than stale_secs.
UPDATE write_event SET created_at = now()
WHERE workspace_id = @workspace_id AND event_key = @event_key AND state = 'pending'
  AND created_at < now() - make_interval(secs => @stale_secs::double precision)
RETURNING *;

-- name: CompleteWriteEvent :exec
UPDATE write_event SET state = 'done', status_code = @status_code, response_body = @response_body
WHERE workspace_id = @workspace_id AND event_key = @event_key AND state = 'pending';

-- name: ReleaseWriteEvent :exec
-- A failed write leaves no trace, so the redelivery can try again.
DELETE FROM write_event WHERE workspace_id = $1 AND event_key = $2 AND state = 'pending';

-- name: DeleteOldWriteEvents :execrows
DELETE FROM write_event WHERE created_at < now() - make_interval(secs => @max_age_secs::double precision);
