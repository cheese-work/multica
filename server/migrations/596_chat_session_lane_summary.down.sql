ALTER TABLE chat_session
    DROP CONSTRAINT IF EXISTS chat_session_lane_summary_length,
    DROP COLUMN IF EXISTS lane_summary_updated_at,
    DROP COLUMN IF EXISTS lane_summary;
