-- CHE-2895: set when a retry claim restamps old human input across the segment
-- floor. A restamped message is never evidence of an answer to a later ask.
ALTER TABLE chat_message ADD COLUMN IF NOT EXISTS reanchored_at TIMESTAMPTZ;
