SET LOCAL lock_timeout = '2s';

-- decision_action: a privileged change an agent prepared and the request's
-- human approves on a decision card. One row per decision request. The
-- payload hash covers params, preconditions, secret slot names and expiry,
-- never a secret value. There is no visible "executing" state: execution and
-- the terminal state commit in one transaction.
CREATE TABLE decision_action (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    decision_request_id UUID NOT NULL,
    action_type TEXT NOT NULL,
    action_version INTEGER NOT NULL CHECK (action_version > 0),
    params JSONB NOT NULL CHECK (jsonb_typeof(params) = 'object'),
    preconditions JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(preconditions) = 'object'),
    secret_slots JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(secret_slots) = 'array'),
    payload_sha256 TEXT NOT NULL CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    expires_at TIMESTAMPTZ NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'executed', 'failed', 'rejected', 'expired', 'cancelled')),
    requested_by_agent_id UUID NOT NULL,
    requested_by_task_id UUID,
    approved_by_user_id UUID,
    approved_at TIMESTAMPTZ,
    executed_at TIMESTAMPTZ,
    webauthn_credential_id UUID,
    result JSONB,
    error_code TEXT,
    error_message TEXT,
    post_commit_done_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT decision_action_executed_at_check CHECK ((state IN ('executed', 'failed')) = (executed_at IS NOT NULL))
);
