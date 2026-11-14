-- Which DM a background task was derived from (CHE-4013 step 9). A background
-- task has no chat_session_id of its own (that column keeps DM serialization
-- and claim rules for chat tasks), so privacy needs a second pointer: the owner
-- of a bg:* task is the creator of this session. No FK on purpose: deleting the
-- DM must not delete or block history rows, and an unresolvable session reads
-- as foreign. NULL for every task that is not DM-derived. Task-copy queries
-- (retry clones, failover, deadline next slot) carry it like purpose.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS source_chat_session_id UUID;
