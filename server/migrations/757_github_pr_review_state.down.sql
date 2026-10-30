ALTER TABLE github_pull_request
    DROP COLUMN IF EXISTS review_state,
    DROP COLUMN IF EXISTS review_head_sha,
    DROP COLUMN IF EXISTS review_id,
    DROP COLUMN IF EXISTS review_at;
