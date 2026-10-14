-- CHE-1665: snapshot_recheck_at is when the slow re-check sweep last picked a
-- PR. Selection skips a PR picked within the re-check TTL even when its fetch
-- failed, so old failing addresses cannot hold the batch caps. A nullable
-- column with no default is catalog-only.
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '10s';

ALTER TABLE github_pull_request ADD COLUMN IF NOT EXISTS snapshot_recheck_at TIMESTAMPTZ;
