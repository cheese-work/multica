CREATE INDEX CONCURRENTLY IF NOT EXISTS issue_wakeup_receipt_head_sha_idx
    ON issue_wakeup_receipt (wakeup_id, (payload->>'head_sha'));
