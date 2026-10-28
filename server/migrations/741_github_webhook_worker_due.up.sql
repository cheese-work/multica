CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_github_webhook_worker_due ON github_webhook_intake (next_attempt_at, received_at) WHERE state = 'deferred';
