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

-- Due pending receipts, oldest due first: a failed receipt moves its own
-- next_attempt_at, so it never holds back the others.
-- name: ListDecisionWakeReceiptCandidates :many
SELECT id FROM decision_wake_receipt
WHERE status = 'pending' AND next_attempt_at <= clock_timestamp()
ORDER BY next_attempt_at, id
LIMIT sqlc.arg('batch_size');

-- Exponential backoff per receipt: 30 s doubling, capped at one hour.
-- name: RecordDecisionWakeReceiptFailure :execrows
UPDATE decision_wake_receipt
SET attempt_count = attempt_count + 1, last_error = sqlc.arg('last_error'),
    next_attempt_at = clock_timestamp() + make_interval(secs => least(3600, 30 * power(2, least(attempt_count, 10))))
WHERE id = sqlc.arg('id') AND status = 'pending';

-- FOR SHARE, not KEY SHARE: an archive is a non-key UPDATE, and only FOR SHARE
-- conflicts with it. An archive that commits after the eligibility reload is
-- therefore impossible: it either committed before this lock (the reload sees
-- it) or waits for this transaction.
-- name: LockDecisionReceiptRecipient :one
SELECT id FROM agent
WHERE workspace_id = sqlc.arg('workspace_id') AND id = sqlc.arg('id')
FOR SHARE;

-- Serializes the receipts of one answer so the last one to settle sees the others.
-- name: LockDecisionAnswerForWake :one
SELECT id FROM decision_answer WHERE id = sqlc.arg('id') FOR UPDATE;

-- The answer's wake outcome once no receipt of the event is pending: the first
-- delivered receipt's outcome (requester before assignee), else skipped.
-- name: SummarizeDecisionWakeEvent :one
SELECT count(*) FILTER (WHERE status = 'pending')::int AS pending,
       coalesce((array_agg(outcome ORDER BY CASE role WHEN 'requester' THEN 0 ELSE 1 END)
                 FILTER (WHERE status = 'delivered' AND outcome IS NOT NULL))[1], 'skipped_ineligible')::text AS outcome
FROM decision_wake_receipt WHERE event_id = sqlc.arg('event_id');
