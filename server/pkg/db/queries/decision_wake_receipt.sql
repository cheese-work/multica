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
-- next_attempt_at, so it never holds back the others. A receipt is not due
-- while its answer's wake_at is NULL (the answering task has not finished) or
-- in the future, or while an older receipt of the same request and recipient
-- is still pending. A withdrawn answer is always selectable: withdrawal clears
-- its wake_at, and delivery must settle its receipts so they stop blocking
-- later ones. Delivery repeats these checks under the locks.
-- name: ListDecisionWakeReceiptCandidates :many
SELECT receipt.id FROM decision_wake_receipt receipt
JOIN decision_wake_event event ON event.id = receipt.event_id
LEFT JOIN decision_answer answer ON answer.id = event.answer_id
WHERE receipt.status = 'pending' AND receipt.next_attempt_at <= clock_timestamp()
  AND (event.answer_id IS NULL OR answer.disclaimed_at IS NOT NULL OR answer.wake_at <= clock_timestamp())
  AND NOT EXISTS (
      SELECT 1 FROM decision_wake_receipt older
      JOIN decision_wake_event older_event ON older_event.id = older.event_id
      WHERE older_event.request_id = event.request_id AND older.recipient_agent_id = receipt.recipient_agent_id
        AND older.status = 'pending' AND older.event_seq < receipt.event_seq)
ORDER BY receipt.next_attempt_at, receipt.id
LIMIT sqlc.arg('batch_size');

-- True while another receipt of the same request and recipient with a lower
-- event_seq is pending, whatever its next_attempt_at: a retraction that backs
-- off still holds back the corrected answer.
-- name: HasOlderPendingDecisionWakeReceipt :one
SELECT EXISTS (
    SELECT 1 FROM decision_wake_receipt older
    JOIN decision_wake_event older_event ON older_event.id = older.event_id
    WHERE older_event.request_id = sqlc.arg('request_id') AND older.recipient_agent_id = sqlc.arg('recipient_agent_id')
      AND older.status = 'pending' AND older.event_seq < sqlc.arg('event_seq'))::boolean;

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

-- Last in the lock order before the receipt.
-- name: LockDecisionWakeEvent :one
SELECT id FROM decision_wake_event WHERE id = sqlc.arg('id') FOR UPDATE;

-- What delivery-time check 3 and the wake_at gate read, under the answer lock.
-- name: GetDecisionAnswerWakeState :one
SELECT (answer.disclaimed_at IS NOT NULL)::boolean AS disclaimed,
       (answer.wake_at IS NULL OR answer.wake_at > clock_timestamp())::boolean AS held,
       coalesce(answer.id = (
           SELECT live.id FROM decision_answer live
           WHERE live.decision_request_id = answer.decision_request_id AND live.disclaimed_at IS NULL
           ORDER BY live.created_at DESC, live.id DESC LIMIT 1), false)::boolean AS latest_live
FROM decision_answer answer WHERE answer.id = sqlc.arg('id');

-- The answer's wake outcome once no receipt of the event is pending: the first
-- delivered receipt's outcome (requester before assignee), else skipped.
-- name: SummarizeDecisionWakeEvent :one
SELECT count(*) FILTER (WHERE status = 'pending')::int AS pending,
       coalesce((array_agg(outcome ORDER BY CASE role WHEN 'requester' THEN 0 ELSE 1 END)
                 FILTER (WHERE status = 'delivered' AND outcome IS NOT NULL))[1], 'skipped_ineligible')::text AS outcome
FROM decision_wake_receipt WHERE event_id = sqlc.arg('event_id');
