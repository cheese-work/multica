ALTER TABLE github_pull_request_check_run
    DROP COLUMN IF EXISTS required,
    DROP COLUMN IF EXISTS red_on_base;
