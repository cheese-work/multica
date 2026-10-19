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
UPDATE push_subscription SET session_valid_until = now()
WHERE user_id = @user_id AND session_id = @session_id;

-- name: DeletePushSubscription :exec
WITH gone AS (
    DELETE FROM push_subscription WHERE id = @id RETURNING id
)
DELETE FROM push_delivery WHERE subscription_id = @id;

-- name: ClaimPushDeliveries :many
-- Leased claim (SKIP LOCKED): another worker cannot take a row until the lease
-- runs out. attempts counts claims, so a crashed holder still uses one up.
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
    attempts = d.attempts + 1
FROM due
WHERE d.id = due.id
RETURNING d.id, d.attempts, now()::timestamptz AS claimed_at;

-- name: GetPushDeliveryContext :one
-- Everything a send needs, and every condition that must still hold: no row
-- means the push is no longer owed. The record is unread, its recipient owns the
-- device (a re-registered device never gets the previous owner's records), the
-- registering session is alive, the user is still a member and the push switch
-- is on. A DM must still be readable; the in_app switch is not part of that, so
-- the type passed to inbox_item_visible is deliberately not dm_reply.
SELECT s.id AS subscription_id, s.endpoint, s.p256dh, s.auth,
       i.id AS record_id, i.type, i.title, i.details, i.chat_session_id,
       w.slug AS workspace_slug,
       COALESCE(w.issue_prefix || '-' || iss.number::text, '')::text AS issue_key,
       COALESCE(a.name, u.name, '')::text AS actor_name
FROM push_delivery d
JOIN push_subscription s ON s.id = d.subscription_id
JOIN inbox_item i ON i.id = d.record_id AND i.recipient_id = s.user_id AND i.recipient_type = 'member'
JOIN workspace w ON w.id = i.workspace_id
JOIN member m ON m.workspace_id = i.workspace_id AND m.user_id = i.recipient_id
LEFT JOIN issue iss ON iss.id = i.issue_id
LEFT JOIN agent a ON i.actor_type = 'agent' AND a.id = i.actor_id
LEFT JOIN "user" u ON i.actor_type = 'member' AND u.id = i.actor_id
WHERE d.id = @id
  AND NOT i.read AND NOT i.archived
  AND s.session_valid_until > now()
  AND COALESCE((
      SELECT p.enabled FROM notification_channel_preference p
       WHERE p.workspace_id = i.workspace_id AND p.user_id = i.recipient_id AND p.channel = 'push'
         AND p.source = CASE WHEN i.chat_session_id IS NULL THEN 'inbox' ELSE 'dm' END
  ), false)
  AND (i.chat_session_id IS NULL
       OR inbox_item_visible('push', i.chat_session_id, i.workspace_id, i.recipient_id));

-- name: FinishPushDelivery :exec
-- A reply that re-armed the row while the push was in flight (next_attempt_at
-- moved past the claim) keeps its schedule and is sent again.
UPDATE push_delivery SET
    claimed_until = NULL,
    sent_at = CASE WHEN next_attempt_at <= @claimed_at THEN now() ELSE NULL END
WHERE id = @id;

-- name: TouchPushSubscription :exec
UPDATE push_subscription SET last_success_at = now() WHERE id = @id;

-- name: RetryPushDelivery :exec
UPDATE push_delivery SET
    claimed_until = NULL,
    next_attempt_at = CASE WHEN next_attempt_at <= @claimed_at THEN now() + make_interval(secs => @backoff_secs::float8) ELSE next_attempt_at END
WHERE id = @id;

-- name: DropPushDelivery :exec
DELETE FROM push_delivery WHERE id = @id AND next_attempt_at <= @claimed_at;

-- name: DeleteSentPushDeliveries :exec
DELETE FROM push_delivery WHERE sent_at < now() - make_interval(secs => @retention_secs::float8);
