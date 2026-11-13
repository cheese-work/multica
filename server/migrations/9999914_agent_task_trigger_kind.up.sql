SET LOCAL lock_timeout = '2s';

-- How trigger_comment_id reached the run. NULL is the ordinary case: the
-- comment (if any) woke the run and its author is the accountable human.
-- 'status_handoff' marks a comment a status move named as handoff context
-- (CHE-5485): the run shows it but its author never becomes the originator.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS trigger_kind TEXT;
