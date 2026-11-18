-- CHE-5404: the GitHub App that produced a Checks API run. NULL for commit
-- statuses and for rows written before this column; the run-budget auto-approver
-- matches an expected check by name and producer, so NULL never satisfies one.
ALTER TABLE github_pull_request_check_run ADD COLUMN IF NOT EXISTS app_id BIGINT;
