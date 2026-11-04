-- CHE-3611: a refused PR keeps who asked for the run, the comment that asked,
-- and the taint the asker carried. The release job starts the run from them,
-- so attribution and taint match the refused run, not the issue's creator with
-- a clean record. No foreign keys.
ALTER TABLE stage_queue_waiting
    ADD COLUMN actor_user_id UUID,
    ADD COLUMN trigger_comment_id UUID,
    ADD COLUMN taint_ids text[] NOT NULL DEFAULT '{}';
