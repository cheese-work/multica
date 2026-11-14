-- Platform writes that carry a stable event id (CHE-5213). A redelivered write
-- replays the stored response instead of writing again. owner is the request
-- that holds the claim; every state change names it.

-- name: ClaimWriteEvent :one
INSERT INTO write_event (workspace_id, event_key, owner) VALUES ($1, $2, $3)
ON CONFLICT (workspace_id, event_key) DO NOTHING
RETURNING *;

-- name: GetWriteEvent :one
SELECT * FROM write_event WHERE workspace_id = $1 AND event_key = $2;

-- name: RetakeStaleWriteEvent :one
-- A claim that never reached its write's transaction (still 'pending') can be
-- taken over once it is older than stale_secs. An 'applied' claim already
-- committed its write and is never retaken.
UPDATE write_event SET owner = @owner, created_at = now()
WHERE workspace_id = @workspace_id AND event_key = @event_key AND state = 'pending'
  AND created_at < now() - make_interval(secs => @stale_secs::double precision)
RETURNING *;

-- name: ApplyWriteEvent :execrows
-- Run inside the write's own transaction: the row lock holds off a competing
-- retake until the write commits or rolls back, and zero rows means this
-- request no longer owns the claim. Idempotent for the owner, so a request
-- with more than one transaction can mark each.
UPDATE write_event SET state = 'applied'
WHERE workspace_id = @workspace_id AND event_key = @event_key AND owner = @owner
  AND state IN ('pending', 'applied');

-- name: CompleteWriteEvent :execrows
UPDATE write_event SET state = 'done', status_code = @status_code, response_body = @response_body
WHERE workspace_id = @workspace_id AND event_key = @event_key AND owner = @owner
  AND state IN ('pending', 'applied');

-- name: ReleaseWriteEvent :exec
-- A failed write that never committed leaves no trace, so the redelivery can
-- try again. An 'applied' claim stays: its write is in the database.
DELETE FROM write_event WHERE workspace_id = $1 AND event_key = $2 AND owner = $3 AND state = 'pending';

-- name: DeleteOldWriteEvents :execrows
DELETE FROM write_event WHERE created_at < now() - make_interval(secs => @max_age_secs::double precision);
