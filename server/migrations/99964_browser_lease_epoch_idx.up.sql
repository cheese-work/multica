-- An epoch is granted once per instance; a reused one would defeat the fence.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS browser_lease_instance_epoch_uidx
    ON browser_lease (instance_id, epoch)
    WHERE epoch IS NOT NULL;
