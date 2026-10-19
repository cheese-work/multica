CREATE INDEX CONCURRENTLY IF NOT EXISTS push_delivery_due_idx ON push_delivery (next_attempt_at) WHERE sent_at IS NULL;
