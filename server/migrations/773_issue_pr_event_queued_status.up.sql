-- CHE-2298 a3: a next_turn event handed to a pending next-turn wake is queued
-- until the wake's run starts and takes it along (delivered).
ALTER TABLE issue_pr_event DROP CONSTRAINT IF EXISTS issue_pr_event_status_check;
ALTER TABLE issue_pr_event ADD CONSTRAINT issue_pr_event_status_check
    CHECK (status IN ('accepted', 'delivering', 'delivered', 'next_turn', 'queued', 'superseded'));
