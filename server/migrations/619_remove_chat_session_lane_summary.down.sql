SET LOCAL lock_timeout = '2s';

ALTER TABLE chat_session
    ADD COLUMN IF NOT EXISTS lane_summary text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS lane_summary_updated_at timestamptz,
    DROP CONSTRAINT IF EXISTS chat_session_lane_summary_length;

ALTER TABLE chat_session
    ADD CONSTRAINT chat_session_lane_summary_length CHECK (char_length(lane_summary) <= 600);
