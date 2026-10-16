SET LOCAL lock_timeout = '2s';

UPDATE comment SET type = 'system' WHERE type = 'jev_route';

ALTER TABLE comment DROP CONSTRAINT IF EXISTS comment_type_check;
ALTER TABLE comment ADD CONSTRAINT comment_type_check
    CHECK (type IN ('comment', 'status_change', 'progress_update', 'system')) NOT VALID;
ALTER TABLE comment VALIDATE CONSTRAINT comment_type_check;
