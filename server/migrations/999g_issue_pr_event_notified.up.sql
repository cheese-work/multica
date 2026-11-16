-- A PR event a member was told about once takes status notified (CHE-5901), so
-- the next-turn bridge stops selecting it while a member holds the issue.
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '10s';

ALTER TABLE issue_pr_event DROP CONSTRAINT issue_pr_event_status_check;
ALTER TABLE issue_pr_event ADD CONSTRAINT issue_pr_event_status_check
    CHECK (status IN ('accepted', 'delivering', 'delivered', 'next_turn', 'superseded', 'notified'));
