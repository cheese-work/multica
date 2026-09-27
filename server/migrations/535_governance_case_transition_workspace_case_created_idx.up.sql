CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_governance_case_transition_workspace_case_created
    ON governance_case_transition (workspace_id, case_id, created_at DESC, id DESC);
