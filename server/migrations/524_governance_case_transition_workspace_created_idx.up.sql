-- Workspace-leading audit pagination; later API code must retain the keyset.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_governance_case_transition_workspace_created
    ON governance_case_transition (workspace_id, created_at DESC, id DESC);
