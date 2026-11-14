ALTER TABLE ci_watch_config
    DROP COLUMN IF EXISTS authorized_at,
    DROP COLUMN IF EXISTS authorized_by_member_id;
