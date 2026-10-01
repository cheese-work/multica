CREATE TABLE IF NOT EXISTS github_merge_announcement (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id       UUID NOT NULL,
    provider           TEXT NOT NULL DEFAULT 'github',
    repository_id      BIGINT NOT NULL,
    repo_owner         TEXT NOT NULL,
    repo_name          TEXT NOT NULL,
    pr_number          INTEGER NOT NULL,
    pull_request_id    UUID NOT NULL,
    issue_id           UUID NOT NULL,
    event_kind         TEXT NOT NULL DEFAULT 'merged',
    delivery_guid      TEXT,
    merge_commit_sha   TEXT NOT NULL,
    merged_at          TIMESTAMPTZ NOT NULL,
    status             TEXT NOT NULL DEFAULT 'pending',
    attempt_count      INTEGER NOT NULL DEFAULT 0,
    last_error         TEXT,
    lease_token        UUID,
    lease_expires_at   TIMESTAMPTZ,
    available_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    comment_id         UUID,
    delivered_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    html_url           TEXT,
    close_intent       BOOLEAN
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_github_merge_announcement_identity
    ON github_merge_announcement (workspace_id, provider, repository_id, pr_number, issue_id, event_kind);

CREATE INDEX IF NOT EXISTS idx_github_merge_announcement_pending_claim
    ON github_merge_announcement (available_at, created_at)
    WHERE status = 'pending';
