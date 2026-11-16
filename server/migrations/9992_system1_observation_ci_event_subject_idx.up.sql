-- CHE-4276: at most one rule row per CI run attempt; the observer relies on it to be idempotent.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS system1_observation_ci_event_subject
    ON system1_observation (workspace_id, subject_id) WHERE kind = 'ci_event';
