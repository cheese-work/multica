SET LOCAL lock_timeout = '2s';

DROP TABLE IF EXISTS decision_snooze;
DROP TABLE IF EXISTS decision_pick;
DROP TABLE IF EXISTS decision_backing_lane;

ALTER TABLE decision_request
    DROP COLUMN IF EXISTS route,
    DROP COLUMN IF EXISTS would_auto_apply,
    DROP COLUMN IF EXISTS auto_apply_rule_id;
ALTER TABLE decision_answer DROP COLUMN IF EXISTS undo_until;

-- Back to the 871 and 873 definitions. NOT VALID: rows written under M1 may hold the
-- new values, and new rows are still checked.
ALTER TABLE decision_answer DROP CONSTRAINT IF EXISTS decision_answer_disclaim_reason_check;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_disclaim_reason_check
    CHECK (disclaim_reason IN ('human', 'answering_turn_cancelled', 'answering_turn_failed', 'source_removed', 'agent_veto', 'record_retracted', 'class_needs_owner', 'card_required')) NOT VALID;

ALTER TABLE decision_answer DROP CONSTRAINT IF EXISTS decision_answer_source_check;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_source_check CHECK (
    (answer_source = 'agent_record' AND answered_by_user_id IS NULL AND (
        (source_comment_id IS NULL AND source_chat_session_id IS NOT NULL AND cardinality(chat_message_ids) > 0)
        OR (source_comment_id IS NOT NULL AND source_chat_session_id IS NULL AND cardinality(chat_message_ids) = 0)))
    OR (answer_source = 'card' AND answered_by_user_id IS NOT NULL AND answered_by_task_id IS NULL AND source_comment_id IS NULL
        AND ((source_chat_session_id IS NULL AND cardinality(chat_message_ids) = 0)
            OR (source_chat_session_id IS NOT NULL AND cardinality(chat_message_ids) > 0)))
) NOT VALID;
