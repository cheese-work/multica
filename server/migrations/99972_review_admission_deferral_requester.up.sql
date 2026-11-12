-- CHE-4651: a deferred reviewer run starts later from its record, so the record
-- keeps who asked (actor, trigger comment) and the taint the asker carried, as
-- stage_queue_waiting does. The release then never runs cleaner or more
-- privileged than the deferred run. No foreign keys.
ALTER TABLE review_admission_deferral
    ADD COLUMN actor_user_id      UUID,
    ADD COLUMN trigger_comment_id UUID,
    ADD COLUMN taint_ids          TEXT[] NOT NULL DEFAULT '{}';
