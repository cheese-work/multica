-- name: ListDecisionExpiryDigestRecipients :many
SELECT DISTINCT request.workspace_id, request.human_user_id
FROM decision_request request
JOIN member ON member.workspace_id = request.workspace_id AND member.user_id = request.human_user_id
WHERE request.authority = 'human' AND request.status = 'cancelled' AND request.cancel_reason = 'expired'
    AND NOT EXISTS (SELECT 1 FROM decision_expiry_digest_request item WHERE item.request_id = request.id)
    AND NOT EXISTS (
        SELECT 1 FROM decision_expiry_digest digest
        WHERE digest.workspace_id = request.workspace_id AND digest.human_user_id = request.human_user_id
            AND digest.digest_day = (clock_timestamp() AT TIME ZONE 'Asia/Bangkok')::date
    )
ORDER BY request.workspace_id, request.human_user_id
LIMIT LEAST(GREATEST(sqlc.arg('batch_size')::int, 0), 100);

-- name: ListDecisionExpiryDigestRequests :many
SELECT request.* FROM decision_request request
WHERE request.authority = 'human' AND request.workspace_id = sqlc.arg('workspace_id') AND request.human_user_id = sqlc.arg('human_user_id')
    AND request.status = 'cancelled' AND request.cancel_reason = 'expired'
    AND NOT EXISTS (SELECT 1 FROM decision_expiry_digest_request item WHERE item.request_id = request.id)
ORDER BY request.created_at, request.id
LIMIT 100
FOR UPDATE OF request;

-- name: DecisionExpiryClock :one
SELECT clock_timestamp()::timestamptz AS now;

-- name: ClaimDecisionExpiryDigest :one
INSERT INTO decision_expiry_digest(id, workspace_id, human_user_id, digest_day)
VALUES (sqlc.arg('id'), sqlc.arg('workspace_id'), sqlc.arg('human_user_id'),
    (sqlc.arg('as_of')::timestamptz AT TIME ZONE 'Asia/Bangkok')::date)
ON CONFLICT (workspace_id, human_user_id, digest_day) DO NOTHING
RETURNING *;

-- name: StampDecisionExpiryDigestInbox :exec
UPDATE decision_expiry_digest SET inbox_item_id = sqlc.arg('inbox_item_id')
WHERE id = sqlc.arg('id');

-- name: StampDecisionExpiryDigestRequests :exec
INSERT INTO decision_expiry_digest_request(request_id, digest_id)
SELECT unnest(sqlc.arg('request_ids')::uuid[]), sqlc.arg('digest_id')::uuid;
