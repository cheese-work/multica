CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS push_delivery_record_subscription_uidx ON push_delivery (record_id, subscription_id);
