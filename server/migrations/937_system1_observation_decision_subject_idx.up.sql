CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS system1_observation_decision_subject
    ON system1_observation (subject_id) WHERE kind = 'decision';
