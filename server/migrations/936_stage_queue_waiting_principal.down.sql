ALTER TABLE stage_queue_waiting
    DROP COLUMN IF EXISTS actor_user_id,
    DROP COLUMN IF EXISTS trigger_comment_id,
    DROP COLUMN IF EXISTS taint_ids;
