SET LOCAL lock_timeout = '2s';

DROP INDEX decision_request_expiry_digest_idx;
DROP INDEX decision_request_expiry_idx;
DROP TABLE decision_expiry_digest_request;
DROP TABLE decision_expiry_digest;

UPDATE decision_request SET cancel_reason = 'agent_withdrawn',
    withdraw_reason = 'Expired after 7 days. Expiry is not an answer or permission to execute.'
WHERE cancel_reason = 'expired';

ALTER TABLE decision_request DROP CONSTRAINT decision_request_cancel_reason_check;
ALTER TABLE decision_request ADD CONSTRAINT decision_request_cancel_reason_check
    CHECK (cancel_reason IN ('issue_closed', 'human_removed', 'agent_archived', 'lane_archived', 'record_disclaimed', 'record_vetoed', 'agent_withdrawn'));
