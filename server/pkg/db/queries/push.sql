-- CHE-1795: Web Push subscriptions and the delivery outbox.

-- name: UpsertPushSubscription :one
-- A device belongs to whoever logged in last: the endpoint is the key, so a new
-- login on the same browser takes the subscription over.
INSERT INTO push_subscription (id, user_id, endpoint, p256dh, auth, session_id, session_valid_until)
VALUES (@id, @user_id, @endpoint, @p256dh, @auth, @session_id, @session_valid_until)
ON CONFLICT (endpoint) DO UPDATE SET
    user_id = EXCLUDED.user_id, p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth,
    session_id = EXCLUDED.session_id, session_valid_until = EXCLUDED.session_valid_until
RETURNING *;

-- name: DeletePushSubscriptionByEndpoint :exec
WITH gone AS (
    DELETE FROM push_subscription WHERE endpoint = @endpoint AND user_id = @user_id RETURNING id
)
DELETE FROM push_delivery WHERE subscription_id IN (SELECT id FROM gone);

-- name: EndPushSession :exec
-- A marker row, not an update of the device rows: logout must not wait behind a
-- send that holds a device (see GetPushDeliveryContext).
INSERT INTO push_session_end (session_id) VALUES (@session_id)
ON CONFLICT (session_id) DO NOTHING;

-- name: PushSessionEnded :one
SELECT EXISTS (SELECT 1 FROM push_session_end WHERE session_id = @session_id)::boolean AS ended;

-- name: DrainPushSession :exec
-- Logout, after its marker is committed: wait until every send that took the
-- shared lock (LockPushSessionShared) before the marker was visible has ended.
-- Past that point no send starts for the session. A send already on the wire
-- cannot be recalled.
SELECT pg_advisory_xact_lock(hashtextextended(@session_id::text, 0));

-- name: LockPushSessionShared :exec
-- Taken by a delivery after the device row and before it reads the context, and
-- held to the end of its transaction, so the marker check and the send form one
-- step as seen by logout (DrainPushSession).
SELECT pg_advisory_xact_lock_shared(hashtextextended(@session_id::text, 0));

-- name: PrunePushSubscription :exec
-- The push service said the endpoint is gone. Only the current claimant of the
-- delivery may act on that: a claimant whose lease expired learned it too late
-- to know what the subscription is now.
WITH claimed AS (
    SELECT 1 FROM push_delivery d WHERE d.id = @delivery_id AND d.claim_token = @claim_token::uuid
), gone AS (
    DELETE FROM push_subscription s WHERE s.id = @id AND EXISTS (SELECT 1 FROM claimed) RETURNING s.id
)
DELETE FROM push_delivery WHERE subscription_id IN (SELECT id FROM gone);

-- name: ClaimPushDeliveries :many
-- Leased claim (SKIP LOCKED): another worker cannot take a row until the lease
-- runs out. attempts counts claims, so a crashed holder still uses one up.
-- claim_token identifies this claim: later statements must present it.
-- claimed_at is the claim's own transaction time; the finishing statements
-- compare next_attempt_at with it to tell "re-armed by a newer reply" apart.
WITH due AS (
    SELECT id FROM push_delivery
    WHERE sent_at IS NULL AND next_attempt_at <= now()
      AND (claimed_until IS NULL OR claimed_until <= now())
    ORDER BY next_attempt_at
    LIMIT @batch FOR UPDATE SKIP LOCKED
)
UPDATE push_delivery d SET
    claimed_until = now() + make_interval(secs => @lease_secs::float8),
    attempts = d.attempts + 1,
    claim_token = gen_random_uuid()
FROM due
WHERE d.id = due.id
RETURNING d.id, d.subscription_id, d.attempts, d.claim_token, now()::timestamptz AS claimed_at;

-- name: LockPushSubscription :one
-- Taken first, in its own statement, so the context below is read after the
-- wait: a statement that waits for a row lock only re-checks that row, not the
-- others it joined. The device row stays locked (FOR NO KEY UPDATE) until the
-- caller's transaction ends, and the caller sends inside it: re-registering the
-- endpoint for another account and pruning wait for the send, so no push starts
-- after the device changes hands and no stale outcome lands on the new owner.
SELECT id, session_id FROM push_subscription WHERE id = @id FOR NO KEY UPDATE;

-- name: GetPushDeliveryContext :one
-- Everything a send needs, and every condition that must still hold: no row
-- means the push is no longer owed. The delivery is unsent and carries the
-- caller's claim token (a claimant whose lease ran out gets nothing). The record
-- is unread, its recipient owns the device (a re-registered device never gets the
-- previous owner's records), the registering session was not ended or expired,
-- the user is still a member and the push switch is on. A DM must still be
-- readable; the in_app switch is not part of that, so the type passed to
-- inbox_item_visible is deliberately not dm_reply.
-- lease_ok: the claim still has min_lease_secs left, enough to finish the send
-- and the writes before anyone else may reclaim the row (clock_timestamp, not
-- now(): the transaction started before the lock wait).
SELECT s.id AS subscription_id, s.endpoint, s.p256dh, s.auth,
       i.id AS record_id, i.type, i.title, i.details, i.chat_session_id,
       w.slug AS workspace_slug,
       COALESCE(w.issue_prefix || '-' || iss.number::text, '')::text AS issue_key,
       COALESCE(a.name, u.name, '')::text AS actor_name,
       (d.claimed_until - clock_timestamp() > make_interval(secs => @min_lease_secs::float8))::boolean AS lease_ok
FROM push_delivery d
JOIN push_subscription s ON s.id = d.subscription_id
JOIN inbox_item i ON i.id = d.record_id AND i.recipient_id = s.user_id AND i.recipient_type = 'member'
JOIN workspace w ON w.id = i.workspace_id
JOIN member m ON m.workspace_id = i.workspace_id AND m.user_id = i.recipient_id
LEFT JOIN issue iss ON iss.id = i.issue_id
LEFT JOIN agent a ON i.actor_type = 'agent' AND a.id = i.actor_id
LEFT JOIN "user" u ON i.actor_type = 'member' AND u.id = i.actor_id
WHERE d.id = @id
  AND d.claim_token = @claim_token::uuid AND d.sent_at IS NULL
  AND NOT i.read AND NOT i.archived
  AND s.session_valid_until > now()
  AND NOT EXISTS (SELECT 1 FROM push_session_end e WHERE e.session_id = s.session_id)
  AND COALESCE((
      SELECT p.enabled FROM notification_channel_preference p
       WHERE p.workspace_id = i.workspace_id AND p.user_id = i.recipient_id AND p.channel = 'push'
         AND p.source = CASE WHEN i.chat_session_id IS NULL THEN 'inbox' ELSE 'dm' END
  ), false)
  AND (i.chat_session_id IS NULL
       OR inbox_item_visible('push', i.chat_session_id, i.workspace_id, i.recipient_id));

-- name: FinishPushDelivery :execrows
-- A reply that re-armed the row while the push was in flight (next_attempt_at
-- moved past the claim) keeps its schedule and is sent again. Zero rows: the
-- claim is no longer ours and nothing was written.
UPDATE push_delivery SET
    claimed_until = NULL,
    claim_token = NULL,
    sent_at = CASE WHEN next_attempt_at <= @claimed_at THEN now() ELSE NULL END
WHERE id = @id AND claim_token = @claim_token::uuid;

-- name: TouchPushSubscription :exec
UPDATE push_subscription SET last_success_at = now() WHERE id = @id;

-- name: RetryPushDelivery :execrows
-- refund: 1 gives the claim's attempt back (the delivery was deferred, not
-- tried), 0 keeps it.
UPDATE push_delivery SET
    claimed_until = NULL,
    claim_token = NULL,
    attempts = GREATEST(attempts - @refund::int, 0),
    next_attempt_at = CASE WHEN next_attempt_at <= @claimed_at THEN now() + make_interval(secs => @backoff_secs::float8) ELSE next_attempt_at END
WHERE id = @id AND claim_token = @claim_token::uuid;

-- name: DropPushDelivery :execrows
DELETE FROM push_delivery WHERE id = @id AND claim_token = @claim_token::uuid AND next_attempt_at <= @claimed_at;

-- name: DeleteSentPushDeliveries :exec
DELETE FROM push_delivery WHERE sent_at < now() - make_interval(secs => @retention_secs::float8);
