ALTER TABLE github_webhook_intake
    ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE TABLE github_webhook_dispatch (
    workspace_id UUID NOT NULL,
    object_kind TEXT NOT NULL CHECK (object_kind IN ('review', 'review_comment', 'issue_comment')),
    object_id BIGINT NOT NULL CHECK (object_id > 0),
    body_hash TEXT NOT NULL CHECK (body_hash ~ '^[0-9a-f]{64}$'),
    issue_id UUID NOT NULL,
    comment_id UUID NOT NULL,
    task_id UUID NOT NULL,
    committed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
