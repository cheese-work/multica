CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS workflow_webhook_endpoint_workflow_name_uidx
    ON workflow_webhook_endpoint (workflow_id, name);
