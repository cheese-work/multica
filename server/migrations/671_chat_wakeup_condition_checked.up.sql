-- When a wakeup's condition was last evaluated. The scheduler checks the least
-- recently checked first, so a long list of unmet conditions cannot starve the
-- ones behind it. Nullable with no default, so the ALTER is metadata-only.
ALTER TABLE chat_wakeup ADD COLUMN IF NOT EXISTS condition_checked_at TIMESTAMPTZ;
