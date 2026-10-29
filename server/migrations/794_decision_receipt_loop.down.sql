SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_answer
    DROP COLUMN IF EXISTS context_message_id,
    DROP COLUMN IF EXISTS reply_text,
    DROP COLUMN IF EXISTS chosen_option_index;
ALTER TABLE decision_request DROP COLUMN IF EXISTS ask_json;
