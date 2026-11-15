-- The pr_merge_owed system rule is a write-ahead intent: a PR merge owes its
-- issue a completion decision until one settles (CHE-5243). Only the
-- issue_wakeup instance constraint changes. The row's (id, revision) is the
-- generation that fences completion; the rule has no stored definitions.
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '10s';

ALTER TABLE issue_wakeup DROP CONSTRAINT issue_wakeup_system_rule_check;
ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_system_rule_check
 CHECK (system_rule IN ('child_done', 'pr_merged', 'pr_checks_failed', 'pr_needs_attention', 'pr_checks_passed', 'workflow_edges', 'pr_dequeued', 'pr_merge_owed'));
