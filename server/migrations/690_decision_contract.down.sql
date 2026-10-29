SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_answer DROP CONSTRAINT IF EXISTS decision_answer_source_check;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_chat_message_ids_check
    CHECK (cardinality(chat_message_ids) > 0) NOT VALID;
