-- github_dispatch_halt_audit: append-only record of every kill switch change
-- (CHE-2223): who flipped which scope, when, and how many live runs it revoked.
CREATE TABLE github_dispatch_halt_audit (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    scope TEXT NOT NULL,
    halted BOOLEAN NOT NULL,
    actor_id UUID,
    actor_source TEXT NOT NULL,
    runs_revoked INTEGER NOT NULL DEFAULT 0,
    sweep_failed BOOLEAN NOT NULL DEFAULT false,
    sweep_pending BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
