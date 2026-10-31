-- Durable claim ledger for gateway-failure recovery (CHE-1126 Layer 2). One row
-- per failed run. Fresh replays and native resume retries share this ledger;
-- admission rules live in the service, serialized per workspace.
-- lineage_root_task_id is the top of the failed run's parent_task_id chain, so
-- native retry accounting is per retry lineage rather than per issue.
CREATE TABLE recovery_claim (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    issue_id UUID NOT NULL,
    failed_task_id UUID NOT NULL,
    lineage_root_task_id UUID NOT NULL,
    mode TEXT NOT NULL CHECK (mode IN ('fresh_replay', 'native_resume', 'native_release')),
    state TEXT NOT NULL DEFAULT 'claimed' CHECK (state IN ('claimed', 'submitted', 'ambiguous', 'completed', 'failed', 'reconciled', 'released')),
    successor_task_id UUID,
    claimed_at TIMESTAMPTZ NOT NULL,
    settled_at TIMESTAMPTZ
);
