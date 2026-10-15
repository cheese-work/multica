SET LOCAL lock_timeout = '2s';

ALTER TABLE github_pull_request DROP COLUMN IF EXISTS snapshot_recheck_at;
