-- One lease holds an instance at a time: granted, or ended but not yet cleanup-acked.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS browser_lease_one_holder_uidx
    ON browser_lease (instance_id)
    WHERE state <> 'queued' AND cleanup_acked_at IS NULL;
