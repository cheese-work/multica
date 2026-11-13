-- What a task is for, beyond who runs it (CHE-2894 ENG-3). NULL is an
-- ordinary task; 'compaction' marks a chat compaction run. Additive and
-- nullable: nothing reads it for routing or authority yet, and every row
-- written before this migration stays NULL. Task-copy queries (retry clones)
-- carry it from the parent so a retry keeps what its parent was for.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS purpose TEXT;
