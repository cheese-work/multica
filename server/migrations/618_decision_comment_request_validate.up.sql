SET LOCAL lock_timeout = '2s';

ALTER TABLE comment VALIDATE CONSTRAINT comment_decision_request_check;
