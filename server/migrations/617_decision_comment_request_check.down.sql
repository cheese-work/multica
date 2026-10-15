SET LOCAL lock_timeout = '2s';

ALTER TABLE comment DROP CONSTRAINT IF EXISTS comment_decision_request_check;
