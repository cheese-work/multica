CREATE INDEX CONCURRENTLY IF NOT EXISTS recovery_claim_workspace_issue_idx
    ON recovery_claim (workspace_id, issue_id);
