CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS workflow_webhook_event_ref_uidx
    ON workflow_webhook_event (endpoint_id, issue_id, ref) WHERE ref IS NOT NULL;
