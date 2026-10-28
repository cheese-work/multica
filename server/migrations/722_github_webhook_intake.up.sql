CREATE TABLE github_webhook_intake (
    delivery_guid TEXT NOT NULL CHECK (delivery_guid <> ''),
    event TEXT NOT NULL CHECK (event IN ('pull_request_review', 'pull_request_review_comment', 'issue_comment')),
    signature TEXT NOT NULL,
    payload BYTEA NOT NULL,
    state TEXT NOT NULL DEFAULT 'deferred' CHECK (state IN ('denied', 'deferred', 'quarantined', 'unsigned', 'projected', 'dispatched')),
    reason TEXT NOT NULL DEFAULT 'awaiting_worker' CHECK (reason <> ''),
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE github_webhook_revision (
    workspace_id UUID NOT NULL,
    installation_id BIGINT NOT NULL,
    repository_id BIGINT NOT NULL,
    pull_request_id UUID,
    pr_number INTEGER NOT NULL,
    object_kind TEXT NOT NULL CHECK (object_kind IN ('review', 'review_comment', 'issue_comment')),
    object_id BIGINT NOT NULL CHECK (object_id > 0),
    body_hash TEXT NOT NULL CHECK (body_hash ~ '^[0-9a-f]{64}$'),
    source_delivery_guid TEXT,
    event TEXT NOT NULL,
    action TEXT NOT NULL,
    sender_id BIGINT NOT NULL,
    author_id BIGINT NOT NULL,
    author_login TEXT NOT NULL,
    author_type TEXT NOT NULL,
    head_sha TEXT NOT NULL,
    review_state TEXT NOT NULL,
    object_created_at TIMESTAMPTZ,
    object_updated_at TIMESTAMPTZ,
    object_submitted_at TIMESTAMPTZ,
    state TEXT NOT NULL CHECK (state IN ('denied', 'deferred', 'quarantined', 'unsigned', 'projected', 'dispatched')),
    reason TEXT NOT NULL CHECK (reason <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
