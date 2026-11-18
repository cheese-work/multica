CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_wakeup_receipt_pr_event_id
    ON issue_wakeup_receipt ((payload->>'event_id'), wakeup_id)
    WHERE event_type = 'pr.event';
