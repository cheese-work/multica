-- CHE-3257: why a failing check is red: 'pr' (the PR caused it), 'infra' (also
-- red on the base tip, or green on another run of the same head), 'optional'
-- (not required by the base branch rules). NULL is unknown: the evidence could
-- not be read, so no cause is attributed.
ALTER TABLE github_pull_request_check_run ADD COLUMN IF NOT EXISTS red_cause TEXT;
