-- CHE-1791: per-channel notification switches, kept apart from the legacy
-- replace-all notification_preference map so an old client's PUT cannot erase
-- them. One row per explicitly set (source, channel); a missing row means the
-- default. The unique index follows in 628.
CREATE TABLE IF NOT EXISTS notification_channel_preference (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    user_id uuid NOT NULL,
    source text NOT NULL CHECK (source IN ('inbox', 'dm')),
    channel text NOT NULL CHECK (channel IN ('in_app', 'push')),
    enabled boolean NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
