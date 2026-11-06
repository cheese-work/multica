-- A DM wakeup may watch one issue instead of only a clock: it fires when the
-- issue gets a matching comment or reaches a status. fire_at is then the
-- expiry; on_timeout says whether expiry wakes the DM or drops the watch.
ALTER TABLE chat_wakeup
    ADD COLUMN IF NOT EXISTS issue_id UUID,
    ADD COLUMN IF NOT EXISTS event_type TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS filter_actor_type TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS filter_actor_id UUID,
    ADD COLUMN IF NOT EXISTS until_status TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS on_timeout TEXT NOT NULL DEFAULT 'drop';
