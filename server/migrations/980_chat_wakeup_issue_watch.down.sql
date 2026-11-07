ALTER TABLE chat_wakeup
    DROP COLUMN IF EXISTS issue_id, DROP COLUMN IF EXISTS event_type, DROP COLUMN IF EXISTS filter_actor_type,
    DROP COLUMN IF EXISTS filter_actor_id, DROP COLUMN IF EXISTS until_status, DROP COLUMN IF EXISTS on_timeout;
