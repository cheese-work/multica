-- Restores the global index. It fails while two workspaces hold the same removal
-- node id, which the restored index forbids; remove those rows to roll back.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS merge_queue_intake_removal_uidx ON merge_queue_intake (removal_node_id) WHERE removal_node_id IS NOT NULL;
