-- CHE-7337: the instant title tier of Cmd+K lists a workspace's issues newest
-- update first and stops at its limit. This index gives it that order, so the
-- walk reads only as many issues as it takes to fill the page.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_workspace_updated_at ON issue (workspace_id, updated_at DESC, id DESC);
