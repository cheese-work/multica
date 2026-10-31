CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS host_sample_key_idx ON host_sample (daemon_id, sampled_at, mount_path);
