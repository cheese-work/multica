-- Idempotency key (observation_id, kind, task_id); a correction in the same task is a different key through supersedes.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS system1_label_labeller_key
    ON system1_label (observation_id, kind, task_id, (coalesce(supersedes, '00000000-0000-0000-0000-000000000000'::uuid)))
    WHERE source = 'labeller';
