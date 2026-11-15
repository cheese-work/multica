CREATE INDEX CONCURRENTLY IF NOT EXISTS task_supplement_replay_due_idx
    ON task_supplement (replay_next_at)
    WHERE failure_reason = 'replay_queued';
