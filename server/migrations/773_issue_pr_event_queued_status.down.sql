UPDATE issue_pr_event SET status = 'next_turn' WHERE status = 'queued';
ALTER TABLE issue_pr_event DROP CONSTRAINT IF EXISTS issue_pr_event_status_check;
ALTER TABLE issue_pr_event ADD CONSTRAINT issue_pr_event_status_check
    CHECK (status IN ('accepted', 'delivering', 'delivered', 'next_turn', 'superseded'));
