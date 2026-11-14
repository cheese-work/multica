SET LOCAL lock_timeout = '2s';

DELETE FROM issue_wakeup WHERE system_rule = 'pr_merge_owed';
ALTER TABLE issue_wakeup DROP CONSTRAINT issue_wakeup_system_rule_check;
ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_system_rule_check
 CHECK (system_rule IN ('child_done', 'pr_merged', 'pr_checks_failed', 'pr_needs_attention', 'pr_checks_passed', 'workflow_edges', 'pr_dequeued'));
