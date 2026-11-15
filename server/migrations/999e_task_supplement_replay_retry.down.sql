ALTER TABLE task_supplement
    DROP COLUMN IF EXISTS replay_next_at,
    DROP COLUMN IF EXISTS replay_attempts;
