SET LOCAL lock_timeout = '2s';

ALTER TABLE browser_profile DROP COLUMN IF EXISTS hosts;
