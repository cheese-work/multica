-- In-place compaction (CHE-2819): the position (created_at, id) of the latest
-- compaction divider. A message is in the current segment iff its position is
-- greater. NULL = never compacted.
ALTER TABLE chat_session ADD COLUMN segment_floor_at TIMESTAMPTZ;
ALTER TABLE chat_session ADD COLUMN segment_floor_id UUID;
