-- name: InsertDecisionWakeEvent :one
INSERT INTO decision_wake_event (id, request_id, seq, type, answer_id, source_comment_id)
VALUES (sqlc.arg('id'), sqlc.arg('request_id'), sqlc.arg('seq'), sqlc.arg('type'), sqlc.narg('answer_id'), sqlc.narg('source_comment_id'))
RETURNING *;

-- name: InsertDecisionWakeReceipt :one
INSERT INTO decision_wake_receipt (id, event_id, event_seq, recipient_agent_id, role, squad_id)
VALUES (sqlc.arg('id'), sqlc.arg('event_id'), sqlc.arg('event_seq'), sqlc.arg('recipient_agent_id'), sqlc.arg('role'), sqlc.narg('squad_id'))
RETURNING *;

-- Callers hold the request row lock (the writer's own update of the request);
-- the lock is what makes seq order equal commit order.
-- name: AllocateDecisionWakeSeq :one
UPDATE decision_request SET wake_seq = wake_seq + 1 WHERE id = sqlc.arg('id') RETURNING wake_seq;

-- name: GetDecisionWakeReceipt :one
SELECT * FROM decision_wake_receipt WHERE id = sqlc.arg('id');

-- name: LockDecisionWakeReceipt :one
SELECT * FROM decision_wake_receipt WHERE id = sqlc.arg('id') FOR UPDATE;

-- name: GetDecisionWakeEvent :one
SELECT * FROM decision_wake_event WHERE id = sqlc.arg('id');

-- Only a pending receipt moves; the row count tells the caller it lost a race.
-- name: SettleDecisionWakeReceipt :execrows
UPDATE decision_wake_receipt
SET status = sqlc.arg('status'), outcome = sqlc.narg('outcome'),
    delivered_at = CASE WHEN sqlc.arg('status') = 'delivered' THEN clock_timestamp() END
WHERE id = sqlc.arg('id') AND status = 'pending';

-- name: GetDecisionRequestForWake :one
SELECT * FROM decision_request WHERE authority = 'human' AND id = sqlc.arg('id');
