SET LOCAL lock_timeout = '2s';

-- excerpted: the record quotes verbatim excerpts of the source message, not the
-- whole message. content_snapshot keeps the whole message either way, so the
-- full text stays one disclosure away. answered_label is the offered-option
-- label (for example D1) that the excerpt answers, read from context_message_id.
ALTER TABLE decision_answer
    ADD COLUMN IF NOT EXISTS excerpted boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS answered_label text;
