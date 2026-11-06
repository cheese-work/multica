CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_run_budget_entry_agent_credit
    ON issue_run_budget_entry (issue_id) WHERE agent_delta <> 0;
