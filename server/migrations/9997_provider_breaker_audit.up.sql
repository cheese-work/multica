-- provider_breaker_audit: append-only record of every breaker transition
-- (CHE-4381): open, reopen, half_open, close and admin reset. actor_id is NULL
-- for automatic transitions and the admin for a reset.
CREATE TABLE provider_breaker_audit (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    provider TEXT NOT NULL,
    event TEXT NOT NULL CHECK (event IN ('open', 'reopen', 'half_open', 'close', 'reset')),
    previous_state TEXT NOT NULL,
    new_state TEXT NOT NULL,
    actor_id UUID,
    task_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
