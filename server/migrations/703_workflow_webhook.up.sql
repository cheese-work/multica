-- CHE-2031: inbound workflow webhook guard. See the security spec in PR #425
-- (docs/engineering/workflow-webhook-guard.md). No foreign keys: the handler
-- resolves workspace, workflow and issue in application code.
CREATE TABLE workflow_webhook_endpoint (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    workflow_id UUID NOT NULL,
    name TEXT NOT NULL,
    secret_sealed BYTEA NOT NULL,
    previous_secret_sealed BYTEA,
    previous_expires_at TIMESTAMPTZ,
    rate_per_min INTEGER NOT NULL DEFAULT 60,
    disabled_at TIMESTAMPTZ,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT workflow_webhook_endpoint_name_check CHECK (name ~ '^[a-z0-9_-]{1,32}$'),
    CONSTRAINT workflow_webhook_endpoint_rate_check CHECK (rate_per_min BETWEEN 1 AND 6000)
);

-- Single-use nonces, kept 900 s. Postgres, not Redis: replay protection must not
-- fail open.
CREATE TABLE workflow_webhook_nonce (
    endpoint_id UUID NOT NULL,
    nonce TEXT NOT NULL,
    seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (endpoint_id, nonce)
);

-- Typed columns only. The raw body is never stored.
CREATE TABLE workflow_webhook_event (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    name TEXT NOT NULL,
    result TEXT NOT NULL,
    ref TEXT,
    body_sha256 BYTEA NOT NULL,
    body_bytes INTEGER NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT workflow_webhook_event_result_check CHECK (result IN ('success', 'failure'))
);
