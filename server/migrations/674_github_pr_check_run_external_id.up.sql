-- The GraphQL node id of a check run or commit status (CheckRun.id,
-- StatusContext.id). CHE-1874: a DM wakeup that waits for "checks finished" needs
-- a durable name for a finished result. Every rerun creates new check runs, and
-- every status posted creates a new status object, so the node id changes with
-- the result even when the name, state, URL and timestamp do not. Nullable with no
-- default, so the ALTER is metadata-only; rows written before this column carry
-- NULL until the next snapshot refresh.
ALTER TABLE github_pull_request_check_run ADD COLUMN IF NOT EXISTS external_id TEXT;
