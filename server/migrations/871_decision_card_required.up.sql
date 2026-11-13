ALTER TABLE decision_answer DROP CONSTRAINT decision_answer_disclaim_reason_check;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_disclaim_reason_check
    CHECK (disclaim_reason IN ('human', 'answering_turn_cancelled', 'answering_turn_failed', 'source_removed', 'agent_veto', 'record_retracted', 'class_needs_owner', 'card_required'));
