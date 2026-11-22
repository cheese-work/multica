-- CHE-7409: a removal's timeline node id is unique per workspace, not globally.
-- Two workspaces can watch the same repository and each keep its own intake
-- row, routing and evidence for the same removal. 99i drops the global index.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS merge_queue_intake_ws_removal_uidx ON merge_queue_intake (workspace_id, removal_node_id) WHERE removal_node_id IS NOT NULL;
