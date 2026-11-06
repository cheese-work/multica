-- The pr_dequeued system rule wakes an issue's owner when GitHub removes a
-- linked PR from the merge queue (CHE-4231). Only the issue_wakeup instance
-- constraint changes: the rule has no stored definitions.
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '10s';

ALTER TABLE issue_wakeup DROP CONSTRAINT issue_wakeup_system_rule_check;
ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_system_rule_check
 CHECK (system_rule IN ('child_done', 'pr_merged', 'pr_checks_failed', 'pr_needs_attention', 'pr_checks_passed', 'workflow_edges', 'pr_dequeued'));
