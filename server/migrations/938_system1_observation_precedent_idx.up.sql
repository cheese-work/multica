CREATE INDEX CONCURRENTLY IF NOT EXISTS system1_observation_precedent
    ON system1_observation (workspace_id, kind, class, created_at DESC);
