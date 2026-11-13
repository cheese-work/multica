SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_request DROP CONSTRAINT decision_request_cancel_reason_check;
ALTER TABLE decision_request ADD CONSTRAINT decision_request_cancel_reason_check
    CHECK (cancel_reason IN ('issue_closed', 'human_removed', 'agent_archived', 'lane_archived', 'record_disclaimed', 'record_vetoed', 'agent_withdrawn', 'expired', 'class_needs_owner', 'delegate_removed', 'agent_unavailable'));
