-- CHE-8956: MarkStartedPREventsDelivered only wants pr.event receipts that name
-- a run (task_id set). Most receipts are processed without one (paused, skipped,
-- coalesced), so the full (event_id, wakeup_id) index made the query read about
-- 200,000 receipts and probe agent_task_queue for each, to match none. This
-- partial index holds only the receipts the query can use.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_wakeup_receipt_pr_event_id_task
    ON issue_wakeup_receipt ((payload->>'event_id'), wakeup_id)
    WHERE event_type = 'pr.event' AND task_id IS NOT NULL;
