CREATE INDEX CONCURRENTLY IF NOT EXISTS workflow_webhook_nonce_endpoint_seen_idx
    ON workflow_webhook_nonce (endpoint_id, seen_at);
