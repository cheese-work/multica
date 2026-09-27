CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_governance_evaluation_workspace_case_captured
    ON governance_evaluation (workspace_id, case_id, captured_at DESC, id DESC);
