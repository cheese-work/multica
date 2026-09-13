-- Durable per-merge announcement record (CHE-374/CHE-379). A linked GitHub PR
-- merging must produce exactly one platform comment on its owning issue,
-- independent of whether the PR declared close intent. No producer existed
-- before this: advanceIssueToDone updates status but never comments, and
-- postChildDoneComment only serves the parent-barrier case.
--
-- No foreign keys per repo convention (see CLAUDE.md) — workspace_id,
-- pull_request_id, and issue_id are validated in application code.
--
-- Identity is deliberately NOT the provider delivery GUID or the merge SHA:
-- GitHub redelivers the same merge under a new X-GitHub-Delivery, so a
-- GUID-keyed row would duplicate the announcement on redelivery. The unique
-- index (see the companion CONCURRENTLY migration) is on
-- (workspace_id, provider, repository_id, pr_number, issue_id, event_kind),
-- which survives redelivery and is per-issue so a PR linked to two issues
-- correctly produces two records/comments.
--
-- delivery_guid and merge_commit_sha are retained as audit fields, not
-- identity. repository_id is GitHub's numeric repo id, captured for identity
-- stability across an owner/repo rename or case variation that the display
-- (repo_owner, repo_name) columns don't guarantee.
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
    -- Delivery/lease state, mirroring webhook_delivery's leasing shape
    -- (176_webhook_delivery_worker.up.sql) so recovery follows an already
    -- reviewed pattern: SKIP LOCKED claim, bounded attempts, sanitized error.
    status             TEXT NOT NULL DEFAULT 'pending',
    attempt_count      INTEGER NOT NULL DEFAULT 0,
    last_error         TEXT,
    lease_token        UUID,
    lease_expires_at   TIMESTAMPTZ,
    available_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    comment_id         UUID,
    delivered_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
