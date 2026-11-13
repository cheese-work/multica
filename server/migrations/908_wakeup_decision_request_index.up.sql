CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_wakeup_decision_request
    ON issue_wakeup (decision_request_id) WHERE enabled AND decision_request_id IS NOT NULL;
