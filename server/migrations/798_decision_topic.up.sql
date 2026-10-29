SET LOCAL lock_timeout = '2s';

-- A decision DM belongs to one topic: the root of the asking issue's parent
-- chain, else its project, else the issue itself. Older DMs keep NULL.
ALTER TABLE chat_session ADD COLUMN IF NOT EXISTS decision_topic_id uuid;
