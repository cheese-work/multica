-- In-place /compact: each compaction bumps the DM's epoch. A chat task takes
-- the epoch it is first dispatched under, so a resume pointer written by a task
-- from an older epoch can be told apart from one started after the summary.
ALTER TABLE chat_session ADD COLUMN compaction_epoch BIGINT NOT NULL DEFAULT 0;
ALTER TABLE agent_task_queue ADD COLUMN chat_compaction_epoch BIGINT;
