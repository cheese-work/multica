-- CHE-3257: which checks the base branch requires, and which of them also fail
-- on the base tip. Existing rows read as required and not red on base, which
-- is how every red was treated before.
ALTER TABLE github_pull_request_check_run
    ADD COLUMN IF NOT EXISTS required    BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN IF NOT EXISTS red_on_base BOOLEAN NOT NULL DEFAULT FALSE;
