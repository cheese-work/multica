CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ops_snapshot_ws_bucket ON ops_snapshot (workspace_id, bucket_ts);
