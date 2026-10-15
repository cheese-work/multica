SET LOCAL lock_timeout = '2s';

ALTER TABLE comment VALIDATE CONSTRAINT comment_type_check;
