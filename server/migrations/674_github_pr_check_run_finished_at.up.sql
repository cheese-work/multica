-- When a check run or commit status finished (GraphQL CheckRun.completedAt,
-- StatusContext.createdAt). CHE-1874: a DM wakeup that waits for "checks
-- finished" needs a durable name for a finished result, also when a rerun keeps
-- the same details URL. Nullable with no default, so the ALTER is metadata-only;
-- rows written before this column carry NULL until the next snapshot refresh.
ALTER TABLE github_pull_request_check_run ADD COLUMN IF NOT EXISTS finished_at TIMESTAMPTZ;
