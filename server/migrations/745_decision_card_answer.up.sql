SET LOCAL lock_timeout = '2s';

-- A card answer is a human click on a decision card, not an agent recording a
-- human reply. It names the approving user, never a task, and has no chat or
-- comment source. Existing rows are all agent_record. An agent_record row may
-- also have no task: the DM card answer (730) is the human's own chat message.
ALTER TABLE decision_answer
    ALTER COLUMN answered_by_task_id DROP NOT NULL,
    ADD COLUMN IF NOT EXISTS answered_by_user_id uuid,
    ADD COLUMN IF NOT EXISTS answer_source text NOT NULL DEFAULT 'agent_record';

ALTER TABLE decision_answer DROP CONSTRAINT IF EXISTS decision_answer_source_check;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_source_check CHECK (
    (answer_source = 'agent_record' AND answered_by_user_id IS NULL AND (
        (source_comment_id IS NULL AND source_chat_session_id IS NOT NULL AND cardinality(chat_message_ids) > 0)
        OR (source_comment_id IS NOT NULL AND source_chat_session_id IS NULL AND cardinality(chat_message_ids) = 0)))
    OR (answer_source = 'card' AND answered_by_user_id IS NOT NULL AND answered_by_task_id IS NULL
        AND source_chat_session_id IS NULL AND source_comment_id IS NULL AND cardinality(chat_message_ids) = 0)
);
