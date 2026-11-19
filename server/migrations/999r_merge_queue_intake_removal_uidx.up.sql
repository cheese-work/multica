CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS merge_queue_intake_removal_uidx ON merge_queue_intake (removal_node_id) WHERE removal_node_id IS NOT NULL;
