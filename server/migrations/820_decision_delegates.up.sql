ALTER TABLE project ADD COLUMN decision_delegates uuid[] NOT NULL DEFAULT '{}';

ALTER TABLE decision_request DROP CONSTRAINT decision_request_cancel_reason_check;
ALTER TABLE decision_request ADD CONSTRAINT decision_request_cancel_reason_check
    CHECK (cancel_reason IN ('issue_closed', 'human_removed', 'agent_archived', 'lane_archived', 'record_disclaimed', 'record_vetoed', 'agent_withdrawn', 'expired', 'class_needs_owner', 'delegate_removed'));

ALTER TABLE decision_answer DROP CONSTRAINT decision_answer_disclaim_reason_check;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_disclaim_reason_check
    CHECK (disclaim_reason IN ('human', 'answering_turn_cancelled', 'answering_turn_failed', 'source_removed', 'agent_veto', 'record_retracted', 'class_needs_owner'));
