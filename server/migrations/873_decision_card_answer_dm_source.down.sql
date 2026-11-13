ALTER TABLE decision_answer DROP CONSTRAINT IF EXISTS decision_answer_source_check;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_source_check CHECK (
    (answer_source = 'agent_record' AND answered_by_user_id IS NULL AND (
        (source_comment_id IS NULL AND source_chat_session_id IS NOT NULL AND cardinality(chat_message_ids) > 0)
        OR (source_comment_id IS NOT NULL AND source_chat_session_id IS NULL AND cardinality(chat_message_ids) = 0)))
    OR (answer_source = 'card' AND answered_by_user_id IS NOT NULL AND answered_by_task_id IS NULL
        AND source_chat_session_id IS NULL AND source_comment_id IS NULL AND cardinality(chat_message_ids) = 0)
) NOT VALID;
