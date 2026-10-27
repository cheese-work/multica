SET LOCAL lock_timeout = '2s';

-- Fails on purpose while any card answer exists: its task id cannot be
-- restored, and deleting a human decision record is not a rollback step.
ALTER TABLE decision_answer DROP CONSTRAINT IF EXISTS decision_answer_source_check;
ALTER TABLE decision_answer
    ALTER COLUMN answered_by_task_id SET NOT NULL,
    DROP COLUMN IF EXISTS answered_by_user_id,
    DROP COLUMN IF EXISTS answer_source;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_source_check CHECK (
    (source_comment_id IS NULL AND source_chat_session_id IS NOT NULL AND cardinality(chat_message_ids) > 0)
    OR (source_comment_id IS NOT NULL AND source_chat_session_id IS NULL AND cardinality(chat_message_ids) = 0)
);
