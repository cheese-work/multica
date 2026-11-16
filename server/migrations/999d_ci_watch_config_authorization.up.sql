SET LOCAL lock_timeout = '2s';

-- CHE-4279: who authorized the watcher to act as its principal. notify_member_id
-- is only a recipient; the watcher acts as that member only while this record
-- names the same member. Written by the settings flow, never inferred.
ALTER TABLE ci_watch_config
    ADD COLUMN IF NOT EXISTS authorized_by_member_id uuid,
    ADD COLUMN IF NOT EXISTS authorized_at timestamptz;
