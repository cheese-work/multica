ALTER TABLE issue_pull_request
    ADD COLUMN IF NOT EXISTS link_kind text NOT NULL DEFAULT 'closing'
    CHECK (link_kind IN ('closing', 'reference'));
