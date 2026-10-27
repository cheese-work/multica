SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_request
    ADD COLUMN IF NOT EXISTS source_comment_id uuid,
    ADD COLUMN IF NOT EXISTS source_content_snapshot text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS request_fingerprint text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS withdraw_reason text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS withdrawn_at timestamptz,
    ADD COLUMN IF NOT EXISTS withdrawn_by_task_id uuid,
    ADD COLUMN IF NOT EXISTS withdrawn_by_user_id uuid;

ALTER TABLE decision_request DROP CONSTRAINT IF EXISTS decision_request_cancel_reason_check;
ALTER TABLE decision_request ADD CONSTRAINT decision_request_cancel_reason_check
    CHECK (cancel_reason IN ('issue_closed', 'human_removed', 'agent_archived', 'lane_archived', 'record_disclaimed', 'record_vetoed', 'agent_withdrawn'));

ALTER TABLE decision_answer
    ADD COLUMN IF NOT EXISTS source_comment_id uuid,
    ALTER COLUMN source_chat_session_id DROP NOT NULL;

ALTER TABLE decision_answer DROP CONSTRAINT IF EXISTS decision_answer_chat_message_ids_check;
ALTER TABLE decision_answer DROP CONSTRAINT IF EXISTS decision_answer_source_check;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_source_check CHECK (
    (source_comment_id IS NULL AND source_chat_session_id IS NOT NULL AND cardinality(chat_message_ids) > 0)
    OR (source_comment_id IS NOT NULL AND source_chat_session_id IS NULL AND cardinality(chat_message_ids) = 0)
);
