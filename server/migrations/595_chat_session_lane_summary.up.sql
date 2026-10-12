ALTER TABLE chat_session
    ADD COLUMN lane_summary text NOT NULL DEFAULT '',
    ADD COLUMN lane_summary_updated_at timestamptz,
    ADD CONSTRAINT chat_session_lane_summary_length CHECK (char_length(lane_summary) <= 600);
