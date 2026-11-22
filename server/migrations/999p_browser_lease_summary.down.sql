SET LOCAL lock_timeout = '2s';

ALTER TABLE browser_lease
    DROP COLUMN IF EXISTS summary_at,
    DROP COLUMN IF EXISTS summary;
