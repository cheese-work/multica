ALTER TABLE ci_watch_action
    DROP COLUMN IF EXISTS followup_at,
    DROP COLUMN IF EXISTS post_outcome;
