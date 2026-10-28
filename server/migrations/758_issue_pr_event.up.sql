-- PR event store (CHE-2298 a1). One row per observed PR event per linked issue.
-- No foreign keys: issue, PR and workspace cleanup stay application-owned.
CREATE TABLE IF NOT EXISTS issue_pr_event (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL,
    issue_id        UUID NOT NULL,
    pull_request_id UUID NOT NULL,
    kind            TEXT NOT NULL
        CHECK (kind IN ('conflict', 'head_changed', 'checks_failed', 'merged', 'closed', 'draft', 'ready')),
    head_sha        TEXT NOT NULL,
    base_sha        TEXT NOT NULL DEFAULT '',
    observed_at     TIMESTAMPTZ NOT NULL,
    dedupe_key      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'accepted'
        CHECK (status IN ('accepted', 'delivering', 'delivered', 'next_turn', 'superseded')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
