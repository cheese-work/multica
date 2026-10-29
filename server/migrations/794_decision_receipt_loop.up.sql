SET LOCAL lock_timeout = '2s';

-- The decoded Ask, kept whole so a card can render options and a receipt can
-- name the chosen one. Older requests keep NULL and render from the markdown.
ALTER TABLE decision_request ADD COLUMN IF NOT EXISTS ask_json jsonb;
-- chosen_option_index is an index into decision_request.ask_json.options, NULL
-- for a free-text answer. reply_text is the human's words without any pasted
-- context; context_message_id links the earlier DM message and quotes nothing.
ALTER TABLE decision_answer
    ADD COLUMN IF NOT EXISTS chosen_option_index integer CHECK (chosen_option_index IS NULL OR chosen_option_index >= 0),
    ADD COLUMN IF NOT EXISTS reply_text text,
    ADD COLUMN IF NOT EXISTS context_message_id uuid;
