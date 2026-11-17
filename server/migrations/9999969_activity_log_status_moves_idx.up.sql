-- CHE-6887: LatestMemberStatusChange, LatestMemberStatusMove and
-- NewestIssueStatusChange look for an issue's newest status move. They walked
-- the issue's activity_log rows newest-first through idx_activity_log_issue_keyset
-- and tested each heap row, so an issue with no match, or only an old one, read
-- all of its rows. This partial index holds only the rows those queries can
-- match, in the same key order, so the walk reads status rows only. The
-- predicate covers `action = 'status_changed'` too, so NewestIssueStatusChange
-- uses it without a query change.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_activity_log_status_moves ON activity_log (issue_id, created_at DESC, id DESC) WHERE action IN ('status_changed', 'run_budget_top_up');
