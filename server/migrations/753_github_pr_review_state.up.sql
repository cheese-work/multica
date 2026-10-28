ALTER TABLE github_pull_request
    ADD COLUMN review_state TEXT NOT NULL DEFAULT '',
    ADD COLUMN review_head_sha TEXT NOT NULL DEFAULT '',
    ADD COLUMN review_at TIMESTAMPTZ;
