-- Extend comment type to allow 'jev_route' for Jev routing announcements (CHE-1708).
SET LOCAL lock_timeout = '2s';

ALTER TABLE comment DROP CONSTRAINT IF EXISTS comment_type_check;
ALTER TABLE comment ADD CONSTRAINT comment_type_check
    CHECK (type IN ('comment', 'status_change', 'progress_update', 'system', 'jev_route')) NOT VALID;
ALTER TABLE comment VALIDATE CONSTRAINT comment_type_check;
