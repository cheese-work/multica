DROP TABLE IF EXISTS ci_watch_event;
ALTER TABLE ci_signature_owner
    DROP COLUMN IF EXISTS check_key,
    DROP COLUMN IF EXISTS last_digest_at,
    DROP COLUMN IF EXISTS digest_class,
    DROP COLUMN IF EXISTS last_run_url,
    DROP COLUMN IF EXISTS fail_count,
    DROP COLUMN IF EXISTS kind;
