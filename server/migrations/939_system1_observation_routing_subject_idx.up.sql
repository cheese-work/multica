CREATE INDEX CONCURRENTLY IF NOT EXISTS system1_observation_routing_subject
    ON system1_observation (subject_id, created_at DESC) WHERE kind = 'routing';
