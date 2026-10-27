CREATE INDEX CONCURRENTLY IF NOT EXISTS workflow_webhook_event_endpoint_issue_idx
    ON workflow_webhook_event (endpoint_id, issue_id, received_at DESC);
