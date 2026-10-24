-- DM wakeup conditions (CHE-1874 PR 2): fire_at becomes the deadline when a
-- condition is set. NULL for a plain time wakeup. Nullable with no default, so
-- the ALTER is metadata-only.
ALTER TABLE chat_wakeup ADD COLUMN IF NOT EXISTS condition JSONB;
