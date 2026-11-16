SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '10s';

UPDATE issue_pr_event SET status = 'next_turn' WHERE status = 'notified';
ALTER TABLE issue_pr_event DROP CONSTRAINT issue_pr_event_status_check;
ALTER TABLE issue_pr_event ADD CONSTRAINT issue_pr_event_status_check
    CHECK (status IN ('accepted', 'delivering', 'delivered', 'next_turn', 'superseded'));
