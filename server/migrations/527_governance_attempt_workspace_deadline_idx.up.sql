-- Claim/reconciliation scans are workspace scoped and deadline ordered.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_governance_attempt_workspace_deadline
    ON governance_attempt (workspace_id, deadline_at, id)
    WHERE terminal_at IS NULL;
