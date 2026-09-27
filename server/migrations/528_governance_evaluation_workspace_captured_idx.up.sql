-- Immutable observation/audit pages remain workspace-leading.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_governance_evaluation_workspace_captured
    ON governance_evaluation (workspace_id, captured_at DESC, id DESC);
