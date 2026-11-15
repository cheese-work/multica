-- CHE-5542: a member steer the turn never read is replayed as a follow-up run
-- on completion. When queueing that run fails for a transient reason, the retry
-- state lives on the receipt so a server restart does not lose it.
ALTER TABLE task_supplement
    ADD COLUMN replay_attempts INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN replay_next_at TIMESTAMPTZ;
